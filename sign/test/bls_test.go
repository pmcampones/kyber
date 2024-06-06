package test

import (
	"testing"

	"go.dedis.ch/kyber/v4/internal/test"
	bls "go.dedis.ch/kyber/v4/pairing/circl_bls12381"
	sign "go.dedis.ch/kyber/v4/sign/bls"
)

func TestBLS12381(t *testing.T) {
	suite := bls.NewSuiteBLS12381()
	scheme := sign.NewSchemeOnG1(suite)
	test.SchemeTesting(t, scheme)
}
