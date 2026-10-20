package jwt_test

import "crypto/x509"

func x509MarshalPKIX(pub any) ([]byte, error) { return x509.MarshalPKIXPublicKey(pub) }
