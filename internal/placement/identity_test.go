package placement

import "testing"

func TestEndpointIdentityIsStableAndCanonical(t *testing.T) {
	firstID, firstLedger, err := EndpointIdentity("https://PVE.Example:8006/")
	if err != nil {
		t.Fatal(err)
	}
	secondID, secondLedger, err := EndpointIdentity("http://pve.example/")
	if err != nil {
		t.Fatal(err)
	}
	if firstID != secondID || firstLedger != secondLedger {
		t.Fatalf("canonical endpoints differ: (%q, %q) != (%q, %q)", firstID, firstLedger, secondID, secondLedger)
	}
	if len(firstLedger) > 63 {
		t.Fatalf("ledger name exceeds DNS label limit: %q", firstLedger)
	}
}

func TestEndpointIdentitySeparatesPorts(t *testing.T) {
	firstID, _, err := EndpointIdentity("https://pve.example:8006")
	if err != nil {
		t.Fatal(err)
	}
	secondID, _, err := EndpointIdentity("https://pve.example:8443")
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatal("different API ports produced the same Proxmox cluster identity")
	}
}

func TestEndpointIdentityRejectsNonEndpointURLs(t *testing.T) {
	for _, endpoint := range []string{
		"not a url", "https://user:pve.example", "https://pve.example/api", "https://pve.example?token=x",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, _, err := EndpointIdentity(endpoint); err == nil {
				t.Fatalf("EndpointIdentity(%q) unexpectedly succeeded", endpoint)
			}
		})
	}
}
