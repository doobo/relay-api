package routes

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestAdminIPAccessSettings(t *testing.T) {
	harness := newAdminHarness(t)

	t.Run("defaults are empty", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/settings/ip-access", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var body ipAccessSettingsDTO
		decodeInto(t, recorder, &body)
		if len(body.Whitelist) != 0 || len(body.Blacklist) != 0 {
			t.Fatalf("default lists = %+v, want empty", body)
		}
		if harness.ipAccess.Load().Active() {
			t.Fatal("default store is active")
		}
	})

	t.Run("invalid entry is rejected", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPut, "/admin/settings/ip-access",
			`{"whitelist":["192.0.2.1","not-an-ip"],"blacklist":[]}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
		}
		if harness.ipAccess.Load().Active() {
			t.Fatal("invalid save changed the live rules")
		}
	})

	t.Run("save normalizes and applies immediately", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPut, "/admin/settings/ip-access",
			`{"whitelist":["192.0.2.0/24","192.0.2.0/24"," 2001:db8::/32 "],
			  "blacklist":["192.0.2.9"]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var body ipAccessSettingsDTO
		decodeInto(t, recorder, &body)
		if len(body.Whitelist) != 2 || body.Whitelist[0] != "192.0.2.0/24" {
			t.Fatalf("whitelist = %v, want deduplicated canonical entries", body.Whitelist)
		}
		if len(body.Blacklist) != 1 || body.Blacklist[0] != "192.0.2.9" {
			t.Fatalf("blacklist = %v", body.Blacklist)
		}

		live := harness.ipAccess.Load()
		if !live.Active() {
			t.Fatal("live store was not updated")
		}
		if live.Allowed(netip.MustParseAddr("192.0.2.9")) {
			t.Fatal("blacklisted address is allowed")
		}
		if !live.Allowed(netip.MustParseAddr("192.0.2.10")) {
			t.Fatal("whitelisted address is denied")
		}
		if live.Allowed(netip.MustParseAddr("198.51.100.1")) {
			t.Fatal("non-whitelisted address is allowed")
		}

		recorder = harness.call(t, http.MethodGet, "/admin/settings/ip-access", "")
		decodeInto(t, recorder, &body)
		if len(body.Whitelist) != 2 || body.UpdatedAt == 0 {
			t.Fatalf("persisted settings = %+v", body)
		}
	})

	t.Run("empty lists clear the rules", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPut, "/admin/settings/ip-access",
			`{"whitelist":[],"blacklist":[]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		if harness.ipAccess.Load().Active() {
			t.Fatal("clearing the lists left the live rules active")
		}
	})
}
