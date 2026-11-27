// Package nettest runs a local tailnet control server, DERP and STUN for
// tests, so no test needs a Tailscale account or the internet (OVERVIEW §7).
// Import it from tests only.
package nettest

import (
	"net/http/httptest"
	"testing"

	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

// MagicDNSDomain is the test tailnet's MagicDNS suffix.
const MagicDNSDomain = "tail-test.ts.net"

// StartControl starts a test control server with in-process DERP and STUN
// on 127.0.0.1. Nodes must present authKey to join. Everything stops at the
// end of the test.
func StartControl(t testing.TB, authKey string) *testcontrol.Server {
	t.Helper()
	derpMap := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	c := &testcontrol.Server{
		DERPMap:        derpMap,
		DNSConfig:      &tailcfg.DNSConfig{Proxied: true},
		MagicDNSDomain: MagicDNSDomain,
		RequireAuthKey: authKey,
		Logf:           logger.Discard,
	}
	c.HTTPTestServer = httptest.NewUnstartedServer(c)
	c.HTTPTestServer.Start()
	t.Cleanup(c.HTTPTestServer.Close)
	return c
}
