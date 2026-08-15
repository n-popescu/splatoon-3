package toyohr

import (
	"context"
	"log"
	"time"

	toyohrv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/toyohr/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NextendoNetwork/splatoon-3/npln/nplnerr"
	"github.com/NextendoNetwork/splatoon-3/npln/services/ugc"
)

// DocumentServices implements the four Splatoon 3 services that publish
// user-generated content: Replay, Locker, Canola and CoopScenario.
//
// They share one shape:
//
//	RegisterDocument   "publish this document I already wrote to the UGC store,
//	                    and (for replays and Salmon Run scenarios) give me the
//	                    short code players type to fetch it"
//	Resolve…Code       "give me the document behind this code"
//	SelectDocuments    "give me the documents matching this query" (lockers and
//	                    ikasu posts, which are browsed rather than coded)
//
// The content itself is never interpreted here. A replay is an opaque blob the
// game wrote; our job is to store it, hand out a code, and give it back intact.
type DocumentServices struct {
	ugc *ugc.Store
}

// NewDocumentServices builds the services on top of the shared UGC store.
func NewDocumentServices(store *ugc.Store) *DocumentServices {
	return &DocumentServices{ugc: store}
}

// Scopes keep the code namespaces apart: a replay code and a Salmon Run scenario
// code may collide as strings without colliding as content.
const (
	scopeReplay       = "replay"
	scopeCoopScenario = "coopScenario"
	scopeLocker       = "locker"
	scopeCanola       = "canola"
)

// ---------------------------------------------------------------------------
// Replay
// ---------------------------------------------------------------------------

// ReplayRegisterDocument publishes a replay and returns its code.
func (d *DocumentServices) ReplayRegisterDocument(ctx context.Context, req *toyohrv1.ReplayRegisterDocumentRequest) (*toyohrv1.ReplayRegisterDocumentResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	doc, ok := d.ugc.Lookup(req.GetName())
	if !ok {
		// The client uploads the document first and registers it second; a
		// missing document means those two steps disagree, which is worth an
		// explicit error rather than a code that resolves to nothing.
		return nil, nplnerr.NotFound("no such document: " + req.GetName())
	}
	if doc.Owner != "" && doc.Owner != caller.UserID {
		return nil, nplnerr.PermissionDenied("that replay belongs to another player")
	}
	code := d.ugc.NewCode()
	d.ugc.PutAlias(ugc.AliasRecord{
		Code:      code,
		Scope:     scopeReplay,
		Document:  req.GetName(),
		Owner:     caller.UserID,
		CreatedAt: time.Now().UTC(),
	})
	log.Printf("[replay] pid=%d published %s as %s", caller.PID, req.GetName(), code)
	return &toyohrv1.ReplayRegisterDocumentResponse{Code: &toyohrv1.ReplayCode{
		Name:      code,
		Document:  req.GetName(),
		Timestamp: timestamppb.New(time.Now()),
	}}, nil
}

// ResolveReplayCode returns the replay behind a code.
func (d *DocumentServices) ResolveReplayCode(ctx context.Context, req *toyohrv1.ResolveReplayCodeRequest) (*toyohrv1.ResolveReplayCodeResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	alias, ok := d.ugc.LookupAlias(req.GetName())
	if !ok || alias.Scope != scopeReplay {
		return nil, nplnerr.NotFound("no replay has that code")
	}
	doc, ok := d.ugc.Lookup(alias.Document)
	if !ok {
		return nil, nplnerr.NotFound("that replay is no longer stored")
	}
	return &toyohrv1.ResolveReplayCodeResponse{Document: d.ugc.Proto(doc)}, nil
}

// FulfillReservations returns the replays queued for a player since the last call.
//
// Retail uses it to deliver replays a player asked to download (from a friend's
// list or a code) in the background. With no reservation queue on a private
// server, the honest answer is the empty list plus the current timestamp — the
// game then simply has nothing new to fetch, instead of erroring.
func (d *DocumentServices) FulfillReservations(ctx context.Context, req *toyohrv1.FulfillReservationsRequest) (*toyohrv1.FulfillReservationsResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	return &toyohrv1.FulfillReservationsResponse{Timestamp: timestamppb.New(time.Now())}, nil
}

// ---------------------------------------------------------------------------
// CoopScenario (Salmon Run scenario codes)
// ---------------------------------------------------------------------------

// CoopScenarioRegisterDocument publishes a Salmon Run scenario.
func (d *DocumentServices) CoopScenarioRegisterDocument(ctx context.Context, req *toyohrv1.CoopScenarioRegisterDocumentRequest) (*toyohrv1.CoopScenarioRegisterDocumentResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := d.ugc.Lookup(req.GetName()); !ok {
		return nil, nplnerr.NotFound("no such document: " + req.GetName())
	}
	code := d.ugc.NewCode()
	d.ugc.PutAlias(ugc.AliasRecord{
		Code:      code,
		Scope:     scopeCoopScenario,
		Document:  req.GetName(),
		Owner:     caller.UserID,
		CreatedAt: time.Now().UTC(),
	})
	log.Printf("[coop] pid=%d published scenario %s as %s", caller.PID, req.GetName(), code)
	return &toyohrv1.CoopScenarioRegisterDocumentResponse{Code: &toyohrv1.CoopScenarioCode{Name: code}}, nil
}

// ResolveCoopScenarioCode returns the scenario behind a code.
func (d *DocumentServices) ResolveCoopScenarioCode(ctx context.Context, req *toyohrv1.ResolveCoopScenarioCodeRequest) (*toyohrv1.ResolveCoopScenarioCodeResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	alias, ok := d.ugc.LookupAlias(req.GetName())
	if !ok || alias.Scope != scopeCoopScenario {
		return nil, nplnerr.NotFound("no scenario has that code")
	}
	doc, ok := d.ugc.Lookup(alias.Document)
	if !ok {
		return nil, nplnerr.NotFound("that scenario is no longer stored")
	}
	return &toyohrv1.ResolveCoopScenarioCodeResponse{Document: d.ugc.Proto(doc)}, nil
}

// ---------------------------------------------------------------------------
// Locker and Canola (browsed content)
// ---------------------------------------------------------------------------

// LockerRegisterDocument publishes a locker.
func (d *DocumentServices) LockerRegisterDocument(ctx context.Context, req *toyohrv1.LockerRegisterDocumentRequest) (*toyohrv1.LockerRegisterDocumentResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	d.publishBrowsable(caller.UserID, scopeLocker, req.GetName())
	log.Printf("[locker] pid=%d published %s", caller.PID, req.GetName())
	return &toyohrv1.LockerRegisterDocumentResponse{}, nil
}

// CanolaRegisterDocument publishes an "ikasu" post.
func (d *DocumentServices) CanolaRegisterDocument(ctx context.Context, req *toyohrv1.CanolaRegisterDocumentRequest) (*toyohrv1.CanolaRegisterDocumentResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	d.publishBrowsable(caller.UserID, scopeCanola, req.GetName())
	log.Printf("[canola] pid=%d published %s (tweet_id=%q)", caller.PID, req.GetName(), req.GetTweetId())
	return &toyohrv1.CanolaRegisterDocumentResponse{}, nil
}

// IkasuDocument marks a post as "ikasu" (the game's like button).
//
// The response message is empty in the protocol definition, so an empty success
// IS the complete answer. The vote itself is recorded as a field transform by the
// client through the UGC store, which is why nothing is written here.
func (d *DocumentServices) IkasuDocument(ctx context.Context, req *toyohrv1.IkasuDocumentRequest) (*toyohrv1.IkasuDocumentResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	log.Printf("[canola] pid=%d liked %s", caller.PID, req.GetName())
	return &toyohrv1.IkasuDocumentResponse{}, nil
}

// SelectDocuments answers the browse queries of Locker and Canola.
//
// The request names a "type" plus free-form parameters; the retail server turns
// that into a query over the content store. We serve the documents of the
// matching scope, newest first — the useful behaviour for "show me lockers /
// posts" — and log the type and parameters so a specific ranking can be
// implemented later against real requests.
func (d *DocumentServices) SelectDocuments(ctx context.Context, req *toyohrv1.SelectDocumentsRequest) (*toyohrv1.SelectDocumentsResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	log.Printf("[ugc] SelectDocuments type=%q params=%d", req.GetType(), len(req.GetParams().GetFields()))
	docs := d.ugc.Browse(scopeFromType(req.GetType()), 24)
	out := &toyohrv1.SelectDocumentsResponse{}
	for _, doc := range docs {
		out.Documents = append(out.Documents, doc)
	}
	return out, nil
}

// publishBrowsable registers a document under a browsable scope (no player-typed
// code, but still addressable).
func (d *DocumentServices) publishBrowsable(owner, scope, document string) {
	d.ugc.PutAlias(ugc.AliasRecord{
		Code:      d.ugc.NewCode(),
		Scope:     scope,
		Document:  document,
		Owner:     owner,
		CreatedAt: time.Now().UTC(),
	})
}

// scopeFromType maps the "type" a SelectDocuments request names onto a scope.
// An unknown type browses everything, which is better than an empty list.
func scopeFromType(t string) string {
	switch {
	case t == "":
		return ""
	case containsFold(t, "locker"):
		return scopeLocker
	case containsFold(t, "ikasu"), containsFold(t, "canola"), containsFold(t, "post"):
		return scopeCanola
	case containsFold(t, "replay"):
		return scopeReplay
	case containsFold(t, "coop"), containsFold(t, "scenario"):
		return scopeCoopScenario
	default:
		return ""
	}
}

// containsFold is a case-insensitive substring test.
func containsFold(haystack, needle string) bool {
	h, n := []rune(lower(haystack)), []rune(lower(needle))
	if len(n) == 0 || len(n) > len(h) {
		return len(n) == 0
	}
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := range n {
			if h[i+j] != n[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}
