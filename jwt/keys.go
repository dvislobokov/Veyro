package jwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
)

// Algorithm is a JWS signing algorithm.
type Algorithm string

// Supported algorithms. "none" is never accepted.
const (
	HS256 Algorithm = "HS256"
	HS384 Algorithm = "HS384"
	HS512 Algorithm = "HS512"
	RS256 Algorithm = "RS256"
	RS384 Algorithm = "RS384"
	RS512 Algorithm = "RS512"
	PS256 Algorithm = "PS256"
	PS384 Algorithm = "PS384"
	PS512 Algorithm = "PS512"
	ES256 Algorithm = "ES256"
	ES384 Algorithm = "ES384"
	ES512 Algorithm = "ES512"
	EdDSA Algorithm = "EdDSA"
)

func (a Algorithm) hash() crypto.Hash {
	switch a {
	case HS256, RS256, PS256, ES256:
		return crypto.SHA256
	case HS384, RS384, PS384, ES384:
		return crypto.SHA384
	case HS512, RS512, PS512, ES512:
		return crypto.SHA512
	}
	return 0
}

// Key is a verification key: an HMAC secret, an RSA, ECDSA or Ed25519
// public key. Algorithms lists what the key may verify; empty means every
// algorithm the key type supports.
type Key struct {
	ID         string
	Algorithms []Algorithm
	Public     any // *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey
	Secret     []byte
}

// HMACKey builds a Key for HS256/HS384/HS512 from a shared secret.
func HMACKey(id string, secret []byte) Key { return Key{ID: id, Secret: secret} }

// PublicKey builds a Key from an RSA, ECDSA or Ed25519 public key.
func PublicKey(id string, pub any) Key { return Key{ID: id, Public: pub} }

// ParsePEM reads a PEM-encoded public key or certificate.
func ParsePEM(id string, pemData []byte) (Key, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return Key{}, errors.New("jwt: no PEM block")
	}
	switch block.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return Key{}, err
		}
		return PublicKey(id, cert.PublicKey), nil
	case "RSA PUBLIC KEY":
		pub, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return Key{}, err
		}
		return PublicKey(id, pub), nil
	default:
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return Key{}, err
		}
		return PublicKey(id, pub), nil
	}
}

// supports reports whether the key can verify alg.
func (k *Key) supports(alg Algorithm) bool {
	if len(k.Algorithms) > 0 {
		found := false
		for _, a := range k.Algorithms {
			if a == alg {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	switch alg {
	case HS256, HS384, HS512:
		return k.Secret != nil
	case RS256, RS384, RS512, PS256, PS384, PS512:
		_, ok := k.Public.(*rsa.PublicKey)
		return ok
	case ES256, ES384, ES512:
		_, ok := k.Public.(*ecdsa.PublicKey)
		return ok
	case EdDSA:
		_, ok := k.Public.(ed25519.PublicKey)
		return ok
	}
	return false
}

// verify checks sig over signingInput with alg.
func (k *Key) verify(alg Algorithm, signingInput, sig []byte) error {
	switch alg {
	case HS256, HS384, HS512:
		h := hmac.New(alg.hash().New, k.Secret)
		h.Write(signingInput)
		if !hmac.Equal(h.Sum(nil), sig) {
			return ErrSignature
		}
		return nil
	case RS256, RS384, RS512:
		digest := digest(alg, signingInput)
		if rsa.VerifyPKCS1v15(k.Public.(*rsa.PublicKey), alg.hash(), digest, sig) != nil {
			return ErrSignature
		}
		return nil
	case PS256, PS384, PS512:
		digest := digest(alg, signingInput)
		opts := &rsa.PSSOptions{SaltLength: alg.hash().Size(), Hash: alg.hash()}
		if rsa.VerifyPSS(k.Public.(*rsa.PublicKey), alg.hash(), digest, sig, opts) != nil {
			return ErrSignature
		}
		return nil
	case ES256, ES384, ES512:
		pub := k.Public.(*ecdsa.PublicKey)
		n := (pub.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*n {
			return ErrSignature
		}
		if !curveMatches(alg, pub.Curve) {
			return ErrSignature
		}
		r := new(big.Int).SetBytes(sig[:n])
		s := new(big.Int).SetBytes(sig[n:])
		if !ecdsa.Verify(pub, digest(alg, signingInput), r, s) {
			return ErrSignature
		}
		return nil
	case EdDSA:
		if !ed25519.Verify(k.Public.(ed25519.PublicKey), signingInput, sig) {
			return ErrSignature
		}
		return nil
	}
	return ErrAlgorithm
}

func curveMatches(alg Algorithm, c elliptic.Curve) bool {
	switch alg {
	case ES256:
		return c == elliptic.P256()
	case ES384:
		return c == elliptic.P384()
	case ES512:
		return c == elliptic.P521()
	}
	return false
}

func digest(alg Algorithm, data []byte) []byte {
	switch alg.hash() {
	case crypto.SHA256:
		d := sha256.Sum256(data)
		return d[:]
	case crypto.SHA384:
		d := sha512.Sum384(data)
		return d[:]
	default:
		d := sha512.Sum512(data)
		return d[:]
	}
}

// ---- JWK ----

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
	K   string `json:"k"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// ParseJWKS decodes a JSON Web Key Set into verification keys. Keys with
// an unsupported type or marked for encryption are skipped.
func ParseJWKS(data []byte) ([]Key, error) {
	var set jwkSet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("jwt: invalid JWKS: %w", err)
	}
	var keys []Key
	for _, j := range set.Keys {
		if j.Use == "enc" {
			continue
		}
		k, err := j.key()
		if err != nil {
			continue
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func (j jwk) key() (Key, error) {
	k := Key{ID: j.Kid}
	if j.Alg != "" {
		k.Algorithms = []Algorithm{Algorithm(j.Alg)}
	}
	switch j.Kty {
	case "RSA":
		n, err := b64(j.N)
		if err != nil {
			return k, err
		}
		e, err := b64(j.E)
		if err != nil {
			return k, err
		}
		k.Public = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	case "EC":
		var curve elliptic.Curve
		switch j.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return k, fmt.Errorf("jwt: unsupported curve %q", j.Crv)
		}
		x, err := b64(j.X)
		if err != nil {
			return k, err
		}
		y, err := b64(j.Y)
		if err != nil {
			return k, err
		}
		k.Public = &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	case "OKP":
		if j.Crv != "Ed25519" {
			return k, fmt.Errorf("jwt: unsupported curve %q", j.Crv)
		}
		x, err := b64(j.X)
		if err != nil {
			return k, err
		}
		k.Public = ed25519.PublicKey(x)
	case "oct":
		s, err := b64(j.K)
		if err != nil {
			return k, err
		}
		k.Secret = s
	default:
		return k, fmt.Errorf("jwt: unsupported key type %q", j.Kty)
	}
	return k, nil
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
