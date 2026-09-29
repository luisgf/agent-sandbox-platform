package mitm

import "testing"

func TestLoadOrGenerateAndIssue(t *testing.T) {
	path := t.TempDir() + "/ca.pem"
	ca, err := LoadOrGenerate(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ca.CertPEM()) == 0 {
		t.Fatal("empty cert pem")
	}
	leaf, err := ca.CertificateForHost("api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Leaf == nil || leaf.Leaf.Subject.CommonName != "api.example.com" {
		t.Fatalf("leaf=%v", leaf.Leaf)
	}
	ca2, err := LoadOrGenerate(path)
	if err != nil {
		t.Fatal(err)
	}
	leaf2, err := ca2.CertificateForHost("api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if leaf2.Leaf.Subject.CommonName != "api.example.com" {
		t.Fatal("reload issue failed")
	}
}
