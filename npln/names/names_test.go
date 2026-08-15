package names

import "testing"

func b() Builder { return Builder{TenantID: "t-dce9377b-lp1"} }

// TestBuildNames checks the resource names we mint against the documented shapes.
func TestBuildNames(t *testing.T) {
	nb := b()
	cases := map[string]string{
		nb.Tenant():                              "tenants/t-dce9377b-lp1",
		nb.User("u-abc"):                         "tenants/t-dce9377b-lp1/users/u-abc",
		nb.Presence("u-abc"):                     "tenants/t-dce9377b-lp1/users/u-abc/presence",
		nb.FriendUser("u-abc", "u-def"):          "tenants/t-dce9377b-lp1/users/u-abc/friendUsers/u-def",
		nb.GameSession("gs-1"):                   "tenants/t-dce9377b-lp1/gameSessions/gs-1",
		nb.UserSession("gs-1", "us-2"):           "tenants/t-dce9377b-lp1/gameSessions/gs-1/userSessions/us-2",
		nb.Account("a-xyz"):                      "accounts/a-xyz",
		nb.IceServerSet("default"):               "tenants/t-dce9377b-lp1/iceServerSets/default",
		nb.SaveRecord("u-abc"):                   "tenants/t-dce9377b-lp1/saveRecords/u-abc",
		nb.VsSchedule("default", "1700000000"):   "tenants/t-dce9377b-lp1/targets/default/vsSchedules/1700000000",
		nb.CoopSchedule("default", "1700000000"): "tenants/t-dce9377b-lp1/targets/default/coopSchedules/1700000000",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// TestParseUserID accepts both the full tenant and the "current" shorthand the
// client is allowed to send — and rejects another tenant's resource.
func TestParseUserID(t *testing.T) {
	nb := b()
	for _, in := range []string{
		"tenants/t-dce9377b-lp1/users/u-abc",
		"tenants/current/users/u-abc",
		"tenants/current/users/u-abc/presence",
		"tenants/current/users/u-abc/friendUsers/u-def",
	} {
		got, err := nb.UserID(in)
		if err != nil {
			t.Fatalf("UserID(%q): %v", in, err)
		}
		if got != "u-abc" {
			t.Errorf("UserID(%q) = %q, want u-abc", in, got)
		}
	}
	for _, bad := range []string{
		"tenants/t-other-lp1/users/u-abc", // another tenant: cross-game leak
		"users/u-abc",
		"tenants/current/gameSessions/gs-1",
		"",
	} {
		if _, err := nb.UserID(bad); err == nil {
			t.Errorf("UserID(%q) was accepted", bad)
		}
	}
}

// TestParseSessionNames covers game-session and user-session parsing.
func TestParseSessionNames(t *testing.T) {
	nb := b()
	if got, err := nb.GameSessionID("tenants/current/gameSessions/gs-1"); err != nil || got != "gs-1" {
		t.Fatalf("GameSessionID = %q, %v", got, err)
	}
	// A user-session name also identifies its game session.
	if got, err := nb.GameSessionID("tenants/current/gameSessions/gs-1/userSessions/us-2"); err != nil || got != "gs-1" {
		t.Fatalf("GameSessionID from a user session = %q, %v", got, err)
	}
	gs, us, err := nb.UserSessionID("tenants/current/gameSessions/gs-1/userSessions/us-2")
	if err != nil || gs != "gs-1" || us != "us-2" {
		t.Fatalf("UserSessionID = %q/%q, %v", gs, us, err)
	}
	if _, _, err := nb.UserSessionID("tenants/current/gameSessions/gs-1"); err == nil {
		t.Error("UserSessionID accepted a game-session name")
	}
}

// TestNormalizeTenant accepts every shorthand the protocol allows.
func TestNormalizeTenant(t *testing.T) {
	nb := b()
	for _, in := range []string{"", "current", "tenants/current", "tenants/t-dce9377b-lp1"} {
		got, err := nb.NormalizeTenant(in)
		if err != nil {
			t.Fatalf("NormalizeTenant(%q): %v", in, err)
		}
		if got != nb.Tenant() {
			t.Errorf("NormalizeTenant(%q) = %q", in, got)
		}
	}
	if _, err := nb.NormalizeTenant("tenants/t-50e39f8f-lp1"); err == nil {
		t.Error("NormalizeTenant accepted another game's tenant")
	}
}

// TestLastSegment is used to read ids out of single-level resource names.
func TestLastSegment(t *testing.T) {
	if got := LastSegment("tenants/current/matchmakingTickets/mt-9"); got != "mt-9" {
		t.Errorf("LastSegment = %q", got)
	}
	if got := LastSegment("bare"); got != "bare" {
		t.Errorf("LastSegment = %q", got)
	}
}
