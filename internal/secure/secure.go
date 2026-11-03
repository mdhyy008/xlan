// Package secure 负责自签证书、指纹校验和首次连接信任（TOFU）。
package secure

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	enabled bool
	pins    *Pins
)

func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Enabled 表示当前是否走加密传输。
func Enabled() bool { return enabled }

// Enable 打开加密传输，并绑定指纹库。
func Enable(p *Pins) {
	enabled = true
	pins = p
}

// Disable 关闭加密（兼容老版本或者自证环境）。
func Disable() {
	enabled = false
}

// Rewrite 把 http:// 换成 https://。
func Rewrite(base string) string {
	if !enabled {
		return base
	}
	return strings.Replace(base, "http://", "https://", 1)
}

// PinsOf 返回当前绑定的指纹库。
func PinsOf() *Pins { return pins }

// Transport 返回一个在握手阶段校验对方证书指纹的 HTTP 传输层。
// 自己发起 TLS 握手，才能拿到「拨号的主机名」作为指纹的键。
func Transport(p *Pins) *http.Transport {
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, ServerName: host}}
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			cs := conn.(*tls.Conn).ConnectionState()
			if len(cs.PeerCertificates) == 0 {
				conn.Close()
				return nil, errors.New("对方没有提供证书")
			}
			if p != nil {
				if err := p.Check(host, Fingerprint(cs.PeerCertificates[0].Raw)); err != nil {
					conn.Close()
					return nil, err
				}
			}
			return conn, nil
		},
	}
}

// ServerTLS 返回服务端配置。
func ServerTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

// LoadOrCreateCert 读取或生成自签证书，返回证书和它的指纹。
func LoadOrCreateCert(dir string) (tls.Certificate, string, error) {
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return c, Fingerprint(c.Certificate[0]), nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "xlan"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"xlan"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, Fingerprint(der), nil
}

// Pins 记录「主机 → 证书指纹」，第一次连上时记住，之后不一致就拒绝。
type Pins struct {
	mu   sync.Mutex
	path string
	m    map[string]string

	// OnNew 在第一次见到某个主机时回调，用来提示用户。
	OnNew func(host, fp string)
}

func LoadPins(path string) *Pins {
	p := &Pins{path: path, m: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &p.m)
	}
	return p
}

func (p *Pins) save() error {
	b, err := json.MarshalIndent(p.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p.path, append(b, '\n'), 0o600)
}

// Check 校验指纹；第一次见到就记下来。
func (p *Pins) Check(host, fp string) error {
	if host == "" {
		return nil
	}
	p.mu.Lock()
	known, ok := p.m[host]
	fresh := !ok
	if fresh {
		p.m[host] = fp
		_ = p.save()
	}
	p.mu.Unlock()

	if fresh {
		if p.OnNew != nil {
			p.OnNew(host, fp)
		}
		return nil
	}
	if known != fp {
		return fmt.Errorf("证书指纹变了！%s\n  之前: %s\n  现在: %s\n如果对方重装过系统，执行 xlan pin forget %s 重新信任；否则可能是有人在冒充",
			host, known[:16], fp[:16], host)
	}
	return nil
}

func (p *Pins) List() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]string, len(p.m))
	for k, v := range p.m {
		out[k] = v
	}
	return out
}

func (p *Pins) Forget(host string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if host == "" {
		p.m = map[string]string{}
	} else {
		delete(p.m, host)
	}
	return p.save()
}
