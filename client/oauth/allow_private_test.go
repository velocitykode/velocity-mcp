package oauth

import "testing"

// TestPrivateHostGuardsDefaultPosture pins the default SSRF posture: plain-HTTP
// non-localhost and private-IP endpoints are rejected during discovery.
func TestPrivateHostGuardsDefaultPosture(t *testing.T) {
	d := NewDiscovery()

	if err := d.requireSecureURL("http://192.168.1.10:4010/.well-known/oauth-protected-resource"); err == nil {
		t.Fatal("plain-HTTP private endpoint accepted by default posture")
	}
	if err := d.requireExternalURL("https://10.0.0.5/token", "https://example.com/mcp"); err == nil {
		t.Fatal("private-IP endpoint accepted by default posture")
	}
	if err := d.requireSecureURL("http://localhost:4010/token"); err != nil {
		t.Fatalf("localhost plain-HTTP rejected: %v", err)
	}
}

// TestAllowPrivateHostsRelaxesGuards covers Config.AllowPrivateHosts: the
// consumer-vouched posture accepts plain-HTTP private endpoints but still
// insists URLs parse.
func TestAllowPrivateHostsRelaxesGuards(t *testing.T) {
	d := NewDiscoveryAllowingPrivateHosts()

	if err := d.requireSecureURL("http://192.168.1.10:4010/.well-known/oauth-protected-resource"); err != nil {
		t.Fatalf("private endpoint rejected despite AllowPrivateHosts: %v", err)
	}
	if err := d.requireExternalURL("http://10.0.0.5/token", "http://192.168.1.10/mcp"); err != nil {
		t.Fatalf("private endpoint rejected despite AllowPrivateHosts: %v", err)
	}
	if err := d.requireSecureURL("not a url"); err == nil {
		t.Fatal("unparseable URL accepted")
	}
}

// TestDiscoveryForConfig pins the Config -> Discovery variant selection.
func TestDiscoveryForConfig(t *testing.T) {
	if discoveryFor(Config{}).allowPrivate {
		t.Fatal("default config must not allow private hosts")
	}
	if !discoveryFor(Config{AllowPrivateHosts: true}).allowPrivate {
		t.Fatal("AllowPrivateHosts config must allow private hosts")
	}
}
