package iosiap

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

// jwsHeader 是 App Store JWS 受保护头（ES256 + x5c）。
type jwsHeader struct {
	Alg string   `json:"alg"`
	X5c []string `json:"x5c"`
}

func parseAndVerifyJWS(token string, roots *x509.CertPool, now time.Time) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, payments.ErrSignatureInvalid
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, payments.ErrSignatureInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, payments.ErrSignatureInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, payments.ErrSignatureInvalid
	}
	var hdr jwsHeader
	if err := json.Unmarshal(headerRaw, &hdr); err != nil {
		return nil, payments.ErrSignatureInvalid
	}
	if !strings.EqualFold(hdr.Alg, "ES256") || len(hdr.X5c) == 0 {
		return nil, payments.ErrSignatureInvalid
	}
	certs := make([]*x509.Certificate, 0, len(hdr.X5c))
	for _, b64 := range hdr.X5c {
		der, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, payments.ErrSignatureInvalid
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, payments.ErrSignatureInvalid
		}
		certs = append(certs, cert)
	}
	leaf := certs[0]
	if now.IsZero() {
		now = time.Now()
	}
	opts := x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}
	if len(certs) > 1 {
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		opts.Intermediates = inter
	}
	if _, err := leaf.Verify(opts); err != nil {
		return nil, payments.ErrSignatureInvalid
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, payments.ErrSignatureInvalid
	}
	if len(sig) != 64 {
		return nil, payments.ErrSignatureInvalid
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], r, s) {
		return nil, payments.ErrSignatureInvalid
	}
	return payload, nil
}

func certPoolFromPEM(pemData string) (*x509.CertPool, error) {
	pemData = strings.TrimSpace(pemData)
	if pemData == "" {
		return nil, fmt.Errorf("empty cert")
	}
	pool := x509.NewCertPool()
	rest := []byte(pemData)
	found := false
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		pool.AddCert(cert)
		found = true
	}
	if !found {
		return nil, fmt.Errorf("no certificates in pem")
	}
	return pool, nil
}

func looksLikeJWS(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "eyJ") {
		return false
	}
	return strings.Count(s, ".") == 2
}
