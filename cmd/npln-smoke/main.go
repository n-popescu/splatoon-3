// Command npln-smoke exercises a running splatoon-3 server over real gRPC.
//
// It is not a unit test: it is the thing you run BEFORE putting a console in
// front of the server, to prove that the transport, the metadata validation, the
// token signing and verification, the per-service authorisation and the schedule
// generation all work together on the wire. Unit tests cannot catch a broken
// interceptor chain or a codec problem; this can, in two seconds.
//
//	go run ./cmd/npln-smoke -addr 127.0.0.1:50051
//
// It asserts the behaviours that must hold, and exits non-zero on the first
// failure:
//
//	1. a request with no npln-tenant-id is refused (Unimplemented, as retail does)
//	2. an anonymous token can be issued
//	3. the social services REFUSE the anonymous user
//	4. the schedule covers now and is contiguous
//	5. a console that resolves to no Nextendo account is refused (fail-closed)
//
// Test 5 is the important one. It is the invariant the Switch friend bug came
// from: a server that answers "some account" instead of an error lets one console
// act as another player.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	authv1 "github.com/n-popescu/splatoon-3/gen/npln/auth/v1"
	friendsv1 "github.com/n-popescu/splatoon-3/gen/npln/friends/v1"
	toyohrv1 "github.com/n-popescu/splatoon-3/gen/npln/toyohr/v1"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "server address (plaintext h2c; run the server with NPLN_DISABLE_TLS=1)")
	tenant := flag.String("tenant", "t-dce9377b-lp1", "tenant id to send in npln-tenant-id")
	nnex := flag.String("nnex", "", "a signed nx2. Nextendo token to embed in the id token, to test a REAL login end to end")
	nsa := flag.String("nsa", "deadbeefdeadbeef", "the NSA/BAAS id to present (an unknown one must be refused)")
	flag.Parse()

	cc, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail("connect: %v", err)
	}
	defer cc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	withTenant := metadata.AppendToOutgoingContext(ctx, "npln-tenant-id", *tenant)

	auth := authv1.NewAuthClient(cc)

	// 1. No tenant metadata: retail answers Unimplemented, and so must we.
	if _, err := auth.IssueAnonymousUserToken(ctx, &authv1.IssueAnonymousUserTokenRequest{Tenant: "tenants/current"}); err == nil {
		fail("a request without npln-tenant-id was ACCEPTED")
	} else if status.Code(err) != codes.Unimplemented {
		fail("missing tenant metadata gave %s, want Unimplemented", status.Code(err))
	}
	pass("a request without npln-tenant-id is refused")

	// 2. Anonymous token.
	anon, err := auth.IssueAnonymousUserToken(withTenant, &authv1.IssueAnonymousUserTokenRequest{
		Tenant:          "tenants/current",
		ExternalIdToken: dummyToken(idToken(*nsa, "")),
	})
	if err != nil {
		fail("IssueAnonymousUserToken: %v", err)
	}
	if anon.GetToken().GetAccessToken() == "" {
		fail("no access token in the anonymous response")
	}
	pass("anonymous token issued for %s", anon.GetToken().GetUser())

	authed := metadata.AppendToOutgoingContext(withTenant, "authorization", "bearer "+anon.GetToken().GetAccessToken())

	// 3. The social services must deny the anonymous user, like retail NPLN.
	if _, err := friendsv1.NewFriendsClient(cc).ListFriendUsers(authed, &friendsv1.ListFriendUsersRequest{}); err == nil {
		fail("Friends served the ANONYMOUS user")
	} else if status.Code(err) != codes.PermissionDenied {
		fail("Friends gave %s for the anonymous user, want PermissionDenied", status.Code(err))
	}
	pass("the anonymous user is refused by Friends")

	// 4. The schedule must cover NOW and be contiguous, or the game will not
	//    enter its online modes.
	sched, err := toyohrv1.NewScheduleClient(cc).SelectVsSchedules(authed, &toyohrv1.SelectVsSchedulesRequest{Target: "default"})
	if err != nil {
		fail("SelectVsSchedules: %v", err)
	}
	slots := sched.GetSchedules()
	if len(slots) == 0 {
		fail("the server served an EMPTY versus schedule; the game would refuse to go online")
	}
	now := time.Now().UTC()
	first := slots[0]
	if now.Before(first.GetStartTime().AsTime()) || now.After(first.GetEndTime().AsTime()) {
		fail("the first slot (%s..%s) does not contain now", first.GetStartTime().AsTime(), first.GetEndTime().AsTime())
	}
	for i := 1; i < len(slots); i++ {
		if !slots[i-1].GetEndTime().AsTime().Equal(slots[i].GetStartTime().AsTime()) {
			fail("slot %d does not start where the previous one ends", i)
		}
	}
	pass("schedule: %d contiguous slots, current one %s..%s, regular stages %v",
		len(slots), first.GetStartTime().AsTime().Format("15:04"), first.GetEndTime().AsTime().Format("15:04"),
		first.GetRegularSettings().GetStages())

	// 5. Fail-closed identity: a console that belongs to no account gets an error,
	//    never somebody else's account.
	_, err = auth.IssuePrearrangedUserToken(withTenant, &authv1.IssuePrearrangedUserTokenRequest{
		Tenant:          "tenants/current",
		UserIndex:       0,
		ExternalIdToken: nsaToken(idToken(*nsa, *nnex)),
	})
	switch {
	case *nnex != "" && err == nil:
		pass("a signed nx2 identity was accepted (a real login works end to end)")
	case *nnex != "" && err != nil:
		fail("the signed nx2 identity was REFUSED: %v", err)
	case err == nil:
		fail("a console with an unknown NSA id (%s) was given a token; identity must fail closed", *nsa)
	case status.Code(err) != codes.Unauthenticated:
		fail("an unknown console gave %s, want Unauthenticated", status.Code(err))
	default:
		pass("a console that resolves to no Nextendo account is refused")
	}

	fmt.Println("\nall checks passed")
}

// idToken builds a BAAS-shaped id token. It is NOT signed: signature verification
// is off by default (NPLN_VERIFY_ID_TOKEN), and what this tool exercises is the
// server's plumbing, not the account layer's crypto.
func idToken(sub, nnex string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"nextendo-baas-key-1"}`))
	claims := map[string]any{
		"sub":      sub,
		"bs:did":   "1234567890abcdef",
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
		"nintendo": map[string]any{"ai": "0100c2500fc20000"},
	}
	if nnex != "" {
		claims["nnex"] = nnex
	}
	body, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + ".not-a-real-signature"
}

func nsaToken(t string) *authv1.ExternalIdToken {
	return &authv1.ExternalIdToken{Token: &authv1.ExternalIdToken_NsaIdToken{NsaIdToken: t}}
}

func dummyToken(t string) *authv1.ExternalIdToken {
	return &authv1.ExternalIdToken{Token: &authv1.ExternalIdToken_DummyExtIdToken{DummyExtIdToken: t}}
}

func pass(format string, args ...any) {
	fmt.Printf("  ok   "+format+"\n", args...)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "  FAIL "+format+"\n", args...)
	os.Exit(1)
}
