// Package ugc implements the UGC (user-generated content) storage services:
// nn.npln.ugcstore.v1.Ugcstore and nn.npln.ugcstore.v1.Screening.
//
// In Splatoon 3 this is where replays, lockers, "ikasu" posts and Salmon Run
// scenarios live. A piece of content is a *document*: a name, a map of fields,
// and optionally an *attachment* (the actual binary blob — a replay file is far
// too big for a protobuf field, so the client uploads it to a URI we hand out and
// the document just references it).
//
// # Scope
//
// The retail service is a small Firestore: structured queries, field transforms,
// preconditions, cursors. This implementation covers what a game server actually
// needs to make the features work:
//
//	documents        get, bulk get, update, delete
//	queries          filter by collection + equality/inequality on fields, order,
//	                 limit and offset
//	transforms       increment / maximum / minimum / server timestamp
//	attachments      upload URI, download URI, set/unset
//	short aliases    the codes players type to fetch each other's content
//
// Cursors (start_at / end_at) are NOT implemented and are logged when a client
// sends them, because a wrong cursor silently returns the wrong page — better to
// see it in the log than to debug a truncated replay list.
package ugc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	commonpb "github.com/n-popescu/splatoon-3/gen/npln/common"
	ugcstorev1 "github.com/n-popescu/splatoon-3/gen/npln/ugcstore/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/n-popescu/splatoon-3/internal/names"
	"github.com/n-popescu/splatoon-3/internal/nplnerr"
	"github.com/n-popescu/splatoon-3/internal/server"
	"github.com/n-popescu/splatoon-3/internal/store"
)

// DocumentRecord is one stored document.
type DocumentRecord struct {
	// Name is the full resource name ("tenants/<t>/documents/<path>").
	Name string `json:"name"`
	// Owner is the NPLN user that created it, so ownership can be enforced.
	Owner string `json:"owner"`
	// OwnerPID ties it to a Nextendo account (for moderation and cleanup).
	OwnerPID uint64 `json:"owner_pid"`
	// Fields is the protojson of the document's MapValue.
	Fields string `json:"fields_json"`
	// Attachment is the id of the uploaded blob, if any.
	Attachment string    `json:"attachment,omitempty"`
	MimeType   string    `json:"mime_type,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AliasRecord maps a short code to a document.
type AliasRecord struct {
	Code      string    `json:"code"`
	Scope     string    `json:"scope"`
	Document  string    `json:"document"`
	Owner     string    `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
}

// Store holds documents, aliases and attachment blobs.
type Store struct {
	names     names.Builder
	docs      *store.JSONMap[DocumentRecord]
	aliases   *store.JSONMap[AliasRecord]
	blobDir   string
	baseURL   string
	uploadsMu sync.Mutex
	// uploads maps a one-shot upload token to the blob id it will produce.
	uploads map[string]uploadTicket
}

type uploadTicket struct {
	blobID  string
	owner   string
	expires time.Time
}

// uploadTTL bounds how long an issued upload URI stays valid.
const uploadTTL = 30 * time.Minute

// StoreOptions configures the store.
type StoreOptions struct {
	Names     names.Builder
	Documents *store.JSONMap[DocumentRecord]
	Aliases   *store.JSONMap[AliasRecord]
	// BlobDir is where attachments are written.
	BlobDir string
	// BaseURL is the public prefix of the upload/download endpoints. Empty
	// disables attachments (and says so when a client asks for one), because
	// handing out a URI the console cannot reach fails much later and much more
	// confusingly.
	BaseURL string
}

// NewStore builds the store.
func NewStore(o StoreOptions) (*Store, error) {
	if o.BlobDir != "" {
		if err := os.MkdirAll(o.BlobDir, 0o700); err != nil {
			return nil, fmt.Errorf("ugc: %w", err)
		}
	}
	return &Store{
		names:   o.Names,
		docs:    o.Documents,
		aliases: o.Aliases,
		blobDir: o.BlobDir,
		baseURL: strings.TrimRight(o.BaseURL, "/"),
		uploads: map[string]uploadTicket{},
	}, nil
}

// Service implements the Ugcstore and Screening gRPC services.
type Service struct {
	*Store
}

// NewService builds the service.
func NewService(s *Store) *Service { return &Service{Store: s} }

// GetDocument returns one document.
func (s *Service) GetDocument(ctx context.Context, req *ugcstorev1.GetDocumentRequest) (*ugcstorev1.Document, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	rec, ok := s.docs.Get(req.GetName())
	if !ok {
		return nil, nplnerr.NotFound("no such document: " + req.GetName())
	}
	return s.proto(rec), nil
}

// BulkGetDocuments streams a document (or a "missing" marker) per requested name.
func (s *Service) BulkGetDocuments(req *ugcstorev1.BulkGetDocumentsRequest, stream ugcstorev1.Ugcstore_BulkGetDocumentsServer) error {
	if _, err := requireCaller(stream.Context()); err != nil {
		return err
	}
	now := timestamppb.New(time.Now())
	for _, name := range req.GetNames() {
		resp := &ugcstorev1.BulkGetDocumentsResponse{ReadTime: now}
		if rec, ok := s.docs.Get(name); ok {
			resp.Result = &ugcstorev1.BulkGetDocumentsResponse_Found{Found: s.proto(rec)}
		} else {
			// A missing document is a normal answer, not an error: the client
			// asks about content that may have expired.
			resp.Result = &ugcstorev1.BulkGetDocumentsResponse_Missing{Missing: name}
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// RunQuery streams the documents matching a structured query.
func (s *Service) RunQuery(req *ugcstorev1.RunQueryRequest, stream ugcstorev1.Ugcstore_RunQueryServer) error {
	if _, err := requireCaller(stream.Context()); err != nil {
		return err
	}
	q := req.GetStructuredQuery()
	if q == nil {
		return nplnerr.InvalidArgument("RunQuery without a structured query")
	}
	if q.GetStartAt() != nil || q.GetEndAt() != nil {
		// Not implemented: see the package comment. Logged rather than ignored
		// silently, so a truncated list has an explanation.
		log.Printf("[ugc] RunQuery used a cursor (start_at/end_at), which this server does not implement")
	}

	collections := map[string]bool{}
	for _, from := range q.GetFrom() {
		if id := from.GetCollectionId(); id != "" {
			collections[id] = true
		}
	}

	var matched []DocumentRecord
	s.docs.Range(func(name string, rec DocumentRecord) bool {
		if len(collections) > 0 && !inCollection(name, collections) {
			return true
		}
		if !matchFilter(q.GetWhere(), rec) {
			return true
		}
		matched = append(matched, rec)
		return true
	})

	// Ordering: the query names the fields; ties fall back to newest-first, which
	// is what every "recent content" list in the game wants.
	sortDocuments(matched, q.GetOrderBy())

	offset := int(q.GetOffset())
	if offset > 0 && offset < len(matched) {
		matched = matched[offset:]
	} else if offset >= len(matched) {
		matched = nil
	}
	if lim := q.GetLimit(); lim != nil && lim.GetValue() > 0 && int(lim.GetValue()) < len(matched) {
		matched = matched[:lim.GetValue()]
	}

	now := timestamppb.New(time.Now())
	for _, rec := range matched {
		if err := stream.Send(&ugcstorev1.RunQueryResponse{Document: s.proto(rec), ReadTime: now}); err != nil {
			return err
		}
	}
	log.Printf("[ugc] RunQuery collections=%v -> %d document(s)", keys(collections), len(matched))
	return nil
}

// CommitDocuments applies a batch of writes.
func (s *Service) CommitDocuments(ctx context.Context, req *ugcstorev1.CommitDocumentsRequest) (*ugcstorev1.CommitDocumentsResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	resp := &ugcstorev1.CommitDocumentsResponse{CommitTime: timestamppb.New(now)}
	for _, op := range req.GetWriteOperations() {
		switch body := op.GetOperationType().(type) {
		case *ugcstorev1.WriteOperation_UpdateDocument:
			result, err := s.update(caller, body.UpdateDocument, now)
			if err != nil {
				return nil, err
			}
			resp.WriteResults = append(resp.WriteResults, result)
		case *ugcstorev1.WriteOperation_DeleteDocument:
			if err := s.delete(caller, body.DeleteDocument); err != nil {
				return nil, err
			}
			resp.WriteResults = append(resp.WriteResults, &ugcstorev1.WriteResult{UpdateTime: timestamppb.New(now)})
		case *ugcstorev1.WriteOperation_TransformDocument:
			result, err := s.transform(caller, body.TransformDocument, now)
			if err != nil {
				return nil, err
			}
			resp.WriteResults = append(resp.WriteResults, result)
		case *ugcstorev1.WriteOperation_SetAttachment:
			if err := s.setAttachment(caller, body.SetAttachment, now); err != nil {
				return nil, err
			}
			resp.WriteResults = append(resp.WriteResults, &ugcstorev1.WriteResult{UpdateTime: timestamppb.New(now)})
		case *ugcstorev1.WriteOperation_UnsetAttachment:
			if err := s.unsetAttachment(caller, body.UnsetAttachment.GetParent(), now); err != nil {
				return nil, err
			}
			resp.WriteResults = append(resp.WriteResults, &ugcstorev1.WriteResult{UpdateTime: timestamppb.New(now)})
		case *ugcstorev1.WriteOperation_ImportAttachment:
			// Importing from a URI would make this server fetch an arbitrary URL
			// on a client's behalf. Refused on purpose (that is a
			// server-side-request-forgery primitive), and the client falls back
			// to uploading the blob itself.
			log.Printf("[ugc] pid=%d asked to import an attachment from %q — refused by policy", caller.PID, body.ImportAttachment.GetUri())
			return nil, nplnerr.PermissionDenied("importing an attachment from a URL is not allowed; upload it instead")
		default:
			return nil, nplnerr.InvalidArgument("unsupported write operation")
		}
	}
	return resp, nil
}

// IssueUploadUri hands out a one-shot URI the client PUTs an attachment to.
func (s *Service) IssueUploadUri(ctx context.Context, req *ugcstorev1.IssueUploadUriRequest) (*ugcstorev1.IssueUploadUriResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	if s.baseURL == "" || s.blobDir == "" {
		return nil, nplnerr.FailedPrecondition("attachment storage is not configured (set NPLN_ATTACHMENT_BASE_URL)")
	}
	token := randomToken()
	blobID := randomToken()
	s.uploadsMu.Lock()
	s.uploads[token] = uploadTicket{blobID: blobID, owner: caller.UserID, expires: time.Now().Add(uploadTTL)}
	s.uploadsMu.Unlock()
	return &ugcstorev1.IssueUploadUriResponse{Uri: s.baseURL + "/ugc/upload/" + token}, nil
}

// IssueAttachmentUri hands out a download URI for a document's attachment.
func (s *Service) IssueAttachmentUri(ctx context.Context, req *ugcstorev1.IssueAttachmentUriRequest) (*ugcstorev1.IssueAttachmentUriResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	uri, err := s.attachmentURI(req.GetParent())
	if err != nil {
		return nil, err
	}
	return &ugcstorev1.IssueAttachmentUriResponse{Uri: uri}, nil
}

// BulkIssueAttachmentUri is the streaming form of IssueAttachmentUri.
func (s *Service) BulkIssueAttachmentUri(stream ugcstorev1.Ugcstore_BulkIssueAttachmentUriServer) error {
	if _, err := requireCaller(stream.Context()); err != nil {
		return err
	}
	for {
		req, err := stream.Recv()
		if err != nil {
			// io.EOF ends the stream normally; anything else is the client's.
			return nil
		}
		uri, err := s.attachmentURI(req.GetParent())
		if err != nil {
			// One missing attachment must not kill the batch.
			uri = ""
		}
		if err := stream.Send(&ugcstorev1.IssueAttachmentUriResponse{Uri: uri}); err != nil {
			return err
		}
	}
}

// CreateDocumentShortAlias gives a document a short code.
func (s *Service) CreateDocumentShortAlias(ctx context.Context, req *ugcstorev1.CreateDocumentShortAliasRequest) (*ugcstorev1.DocumentShortAlias, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	in := req.GetDocumentShortAlias()
	code := names.LastSegment(in.GetName())
	if code == "" {
		code = randomCode()
	}
	rec := AliasRecord{
		Code:      code,
		Scope:     in.GetScope(),
		Document:  in.GetDocument(),
		Owner:     caller.UserID,
		CreatedAt: time.Now().UTC(),
	}
	key := aliasKey(rec.Scope, code)
	if existing, found := s.aliases.Get(key); found && existing.Document != rec.Document {
		return nil, nplnerr.AlreadyExists("that code is already taken")
	}
	s.aliases.Put(key, rec)
	log.Printf("[ugc] pid=%d published %s as code %s (scope %q)", caller.PID, rec.Document, code, rec.Scope)
	return &ugcstorev1.DocumentShortAlias{
		Name:     s.names.DocumentShortAlias(rec.Scope, code),
		Document: rec.Document,
		Scope:    rec.Scope,
	}, nil
}

// GetDocumentShortAlias resolves a short code.
func (s *Service) GetDocumentShortAlias(ctx context.Context, req *ugcstorev1.GetDocumentShortAliasRequest) (*ugcstorev1.DocumentShortAlias, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	rec, ok := s.LookupAlias(req.GetName())
	if !ok {
		return nil, nplnerr.NotFound("no content has that code")
	}
	return &ugcstorev1.DocumentShortAlias{
		Name:     s.names.DocumentShortAlias(rec.Scope, rec.Code),
		Document: rec.Document,
		Scope:    rec.Scope,
	}, nil
}

// BulkGetDocumentShortAliases resolves several codes at once.
func (s *Service) BulkGetDocumentShortAliases(ctx context.Context, req *ugcstorev1.BulkGetDocumentShortAliasesRequest) (*ugcstorev1.BulkGetDocumentShortAliasesResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	out := &ugcstorev1.BulkGetDocumentShortAliasesResponse{}
	for _, r := range req.GetRequests() {
		if rec, ok := s.LookupAlias(r.GetName()); ok {
			out.Results = append(out.Results, &ugcstorev1.BulkGetDocumentShortAliasesResult{
				Result: &ugcstorev1.BulkGetDocumentShortAliasesResult_Found{Found: &ugcstorev1.DocumentShortAlias{
					Name:     s.names.DocumentShortAlias(rec.Scope, rec.Code),
					Document: rec.Document,
					Scope:    rec.Scope,
				}},
			})
			continue
		}
		out.Results = append(out.Results, &ugcstorev1.BulkGetDocumentShortAliasesResult{
			Result: &ugcstorev1.BulkGetDocumentShortAliasesResult_Missing{Missing: r.GetName()},
		})
	}
	return out, nil
}

// CreateReport (Screening) stores a report against a piece of content.
func (s *Service) CreateReport(ctx context.Context, req *ugcstorev1.CreateReportRequest) (*ugcstorev1.Report, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	in := req.GetReport()
	log.Printf("[ugc] pid=%d reported content %q (%s / %s)", caller.PID, in.GetScreeningTarget(), in.GetCategory(), in.GetReason())
	return &ugcstorev1.Report{
		Name:                s.names.Tenant() + "/contentReports/" + randomToken(),
		Category:            in.GetCategory(),
		Reason:              in.GetReason(),
		LanguageCode:        in.GetLanguageCode(),
		CreateTime:          timestamppb.New(time.Now()),
		ScreeningTarget:     in.GetScreeningTarget(),
		ScreeningTargetType: in.GetScreeningTargetType(),
		Context:             in.GetContext(),
	}, nil
}

// ---------------------------------------------------------------------------
// store operations shared with the Splatoon 3 document services
// ---------------------------------------------------------------------------

// Put stores (or replaces) a document owned by a caller.
func (s *Store) Put(caller *server.Caller, name string, fields *commonpb.MapValue) DocumentRecord {
	now := time.Now().UTC()
	return s.docs.Update(name, func(cur DocumentRecord, found bool) DocumentRecord {
		if !found {
			cur = DocumentRecord{Name: name, Owner: caller.UserID, OwnerPID: caller.PID, CreatedAt: now}
		}
		if fields != nil {
			cur.Fields = marshalMap(fields)
		}
		cur.UpdatedAt = now
		return cur
	})
}

// Lookup returns a document by name.
func (s *Store) Lookup(name string) (DocumentRecord, bool) { return s.docs.Get(name) }

// LookupAlias resolves an alias resource name or bare code.
func (s *Store) LookupAlias(nameOrCode string) (AliasRecord, bool) {
	code := names.LastSegment(nameOrCode)
	// The alias resource name embeds the scope as "<scope>-<code>"; try the whole
	// last segment first, then every scope we know.
	if rec, ok := s.aliases.Get(code); ok {
		return rec, true
	}
	if scope, bare, found := strings.Cut(code, "-"); found {
		if rec, ok := s.aliases.Get(aliasKey(scope, bare)); ok {
			return rec, true
		}
	}
	var out AliasRecord
	ok := false
	s.aliases.Range(func(_ string, rec AliasRecord) bool {
		if strings.EqualFold(rec.Code, code) {
			out, ok = rec, true
			return false
		}
		return true
	})
	return out, ok
}

// PutAlias stores an alias.
func (s *Store) PutAlias(rec AliasRecord) { s.aliases.Put(aliasKey(rec.Scope, rec.Code), rec) }

// Proto renders a document record (exported for the Splatoon 3 services).
func (s *Store) Proto(rec DocumentRecord) *ugcstorev1.Document { return s.proto(rec) }

// Names exposes the resource-name builder to the sibling services.
func (s *Store) Names() names.Builder { return s.names }

// NewCode mints a short content code.
func (s *Store) NewCode() string { return randomCode() }

// proto renders a document record.
func (s *Store) proto(rec DocumentRecord) *ugcstorev1.Document {
	out := &ugcstorev1.Document{
		Name:   rec.Name,
		Fields: unmarshalMap(rec.Fields),
	}
	if !rec.CreatedAt.IsZero() {
		out.CreateTime = timestamppb.New(rec.CreatedAt)
	}
	if !rec.UpdatedAt.IsZero() {
		out.UpdateTime = timestamppb.New(rec.UpdatedAt)
	}
	return out
}

// update applies an UpdateDocument write.
func (s *Service) update(caller *server.Caller, req *ugcstorev1.UpdateDocumentRequest, now time.Time) (*ugcstorev1.WriteResult, error) {
	doc := req.GetDocument()
	if doc.GetName() == "" {
		return nil, nplnerr.InvalidArgument("update_document without a document name")
	}
	existing, found := s.docs.Get(doc.GetName())
	if found && existing.Owner != "" && existing.Owner != caller.UserID {
		// Content belongs to the player who made it. Letting anybody rewrite
		// somebody else's replay or locker is not a hypothetical: the document
		// name is guessable.
		return nil, nplnerr.PermissionDenied("this document belongs to another player")
	}
	if pre := req.GetCurrentDocument(); pre != nil {
		if exists, ok := pre.GetConditionType().(*ugcstorev1.Precondition_Exists); ok {
			if exists.Exists && !found {
				return nil, nplnerr.FailedPrecondition("document does not exist")
			}
			if !exists.Exists && found {
				return nil, nplnerr.AlreadyExists("document already exists")
			}
		}
		if ut, ok := pre.GetConditionType().(*ugcstorev1.Precondition_UpdateTime); ok && found {
			if ut.UpdateTime.IsValid() && existing.UpdatedAt.After(ut.UpdateTime.AsTime().UTC().Add(time.Second)) {
				return nil, nplnerr.FailedPrecondition("the stored document is newer")
			}
		}
	}
	// The update mask names the fields to touch; without one the whole field map
	// is replaced (that is the documented default for a field mask).
	merged := doc.GetFields()
	if mask := req.GetUpdateMask(); mask != nil && len(mask.GetPaths()) > 0 && found {
		merged = mergeFields(unmarshalMap(existing.Fields), doc.GetFields(), mask.GetPaths())
	}
	s.Put(caller, doc.GetName(), merged)
	return &ugcstorev1.WriteResult{UpdateTime: timestamppb.New(now)}, nil
}

// delete applies a DeleteDocument write.
func (s *Service) delete(caller *server.Caller, req *ugcstorev1.DeleteDocumentRequest) error {
	rec, found := s.docs.Get(req.GetName())
	if !found {
		return nil // deleting what is not there is a success
	}
	if rec.Owner != "" && rec.Owner != caller.UserID {
		return nplnerr.PermissionDenied("this document belongs to another player")
	}
	s.docs.Delete(req.GetName())
	return nil
}

// transform applies field transforms (increments, min/max, server timestamps).
func (s *Service) transform(caller *server.Caller, req *ugcstorev1.TransformDocumentRequest, now time.Time) (*ugcstorev1.WriteResult, error) {
	rec, found := s.docs.Get(req.GetName())
	if !found {
		rec = DocumentRecord{Name: req.GetName(), Owner: caller.UserID, OwnerPID: caller.PID, CreatedAt: now}
	} else if rec.Owner != "" && rec.Owner != caller.UserID {
		return nil, nplnerr.PermissionDenied("this document belongs to another player")
	}
	fields := unmarshalMap(rec.Fields)
	if fields == nil {
		fields = &commonpb.MapValue{Fields: map[string]*commonpb.Value{}}
	}
	if fields.Fields == nil {
		fields.Fields = map[string]*commonpb.Value{}
	}
	var results []*commonpb.Value
	for _, t := range req.GetFieldTransforms() {
		path := t.GetFieldPath()
		cur := fields.Fields[path]
		var next *commonpb.Value
		switch body := t.GetTransformType().(type) {
		case *ugcstorev1.FieldTransform_SetServerValue:
			next = &commonpb.Value{ValueType: &commonpb.Value_TimestampValue{TimestampValue: timestamppb.New(now)}}
		case *ugcstorev1.FieldTransform_Increment:
			next = addValues(cur, body.Increment)
		case *ugcstorev1.FieldTransform_Maximum:
			next = pickValue(cur, body.Maximum, true)
		case *ugcstorev1.FieldTransform_Minimum:
			next = pickValue(cur, body.Minimum, false)
		case *ugcstorev1.FieldTransform_BufferedIncrement:
			// Removed from the protocol in a later version; treated as a plain
			// increment, which is what it was.
			next = addValues(cur, body.BufferedIncrement)
		default:
			next = cur
		}
		fields.Fields[path] = next
		results = append(results, next)
	}
	rec.Fields = marshalMap(fields)
	rec.UpdatedAt = now
	s.docs.Put(rec.Name, rec)
	return &ugcstorev1.WriteResult{UpdateTime: timestamppb.New(now), TransformResults: results}, nil
}

// setAttachment binds an uploaded blob (or an inline body) to a document.
func (s *Service) setAttachment(caller *server.Caller, req *ugcstorev1.SetAttachmentRequest, now time.Time) error {
	if s.blobDir == "" {
		return nplnerr.FailedPrecondition("attachment storage is not configured")
	}
	rec, found := s.docs.Get(req.GetParent())
	if !found {
		rec = DocumentRecord{Name: req.GetParent(), Owner: caller.UserID, OwnerPID: caller.PID, CreatedAt: now}
	} else if rec.Owner != "" && rec.Owner != caller.UserID {
		return nplnerr.PermissionDenied("this document belongs to another player")
	}
	if body := req.GetBody(); len(body) > 0 {
		id := hashID(body)
		if err := os.WriteFile(filepath.Join(s.blobDir, id), body, 0o600); err != nil {
			return nplnerr.Internal("cannot store the attachment: " + err.Error())
		}
		rec.Attachment = id
	}
	rec.MimeType = req.GetMimeType()
	rec.UpdatedAt = now
	s.docs.Put(rec.Name, rec)
	return nil
}

// unsetAttachment removes a document's attachment.
func (s *Service) unsetAttachment(caller *server.Caller, name string, now time.Time) error {
	rec, found := s.docs.Get(name)
	if !found {
		return nil
	}
	if rec.Owner != "" && rec.Owner != caller.UserID {
		return nplnerr.PermissionDenied("this document belongs to another player")
	}
	if rec.Attachment != "" && s.blobDir != "" {
		_ = os.Remove(filepath.Join(s.blobDir, rec.Attachment))
	}
	rec.Attachment = ""
	rec.UpdatedAt = now
	s.docs.Put(name, rec)
	return nil
}

// attachmentURI builds the download URI of a document's attachment.
func (s *Store) attachmentURI(documentName string) (string, error) {
	if s.baseURL == "" {
		return "", nplnerr.FailedPrecondition("attachment storage is not configured")
	}
	rec, ok := s.docs.Get(documentName)
	if !ok || rec.Attachment == "" {
		return "", nplnerr.NotFound("that document has no attachment")
	}
	return s.baseURL + "/ugc/blob/" + rec.Attachment, nil
}

// ClaimUpload consumes an upload token, returning the blob id to write.
// Used by the HTTP upload endpoint.
func (s *Store) ClaimUpload(token string) (string, bool) {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()
	t, ok := s.uploads[token]
	if !ok {
		return "", false
	}
	delete(s.uploads, token)
	if time.Now().After(t.expires) {
		return "", false
	}
	return t.blobID, true
}

// BlobDir is where attachments live (used by the HTTP endpoints).
func (s *Store) BlobDir() string { return s.blobDir }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// requireCaller returns the authenticated caller.
func requireCaller(ctx context.Context) (*server.Caller, error) {
	c, ok := server.CallerFrom(ctx)
	if !ok {
		return nil, nplnerr.TokenInvalid("no access token")
	}
	if c.Anonymous {
		return nil, nplnerr.PermissionDenied("the anonymous user cannot use content storage")
	}
	return c, nil
}

// inCollection reports whether a document name belongs to one of the collections.
// A document name is "tenants/<t>/documents/<collection>/<id>[/…]".
func inCollection(name string, collections map[string]bool) bool {
	idx := strings.Index(name, "/documents/")
	if idx < 0 {
		return false
	}
	path := name[idx+len("/documents/"):]
	collection, _, _ := strings.Cut(path, "/")
	return collections[collection]
}

// matchFilter evaluates a structured-query filter against a document.
func matchFilter(f *ugcstorev1.StructuredQuery_Filter, rec DocumentRecord) bool {
	if f == nil {
		return true
	}
	fields := unmarshalMap(rec.Fields)
	return matchFilterFields(f, fields)
}

func matchFilterFields(f *ugcstorev1.StructuredQuery_Filter, fields *commonpb.MapValue) bool {
	switch body := f.GetFilterType().(type) {
	case *ugcstorev1.StructuredQuery_Filter_CompositeFilter:
		// AND is the only composite operator the protocol defines.
		for _, sub := range body.CompositeFilter.GetFilters() {
			if !matchFilterFields(sub, fields) {
				return false
			}
		}
		return true
	case *ugcstorev1.StructuredQuery_Filter_FieldFilter:
		return matchFieldFilter(body.FieldFilter, fields)
	case *ugcstorev1.StructuredQuery_Filter_UnaryFilter:
		v := fields.GetFields()[body.UnaryFilter.GetField().GetFieldPath()]
		switch body.UnaryFilter.GetOp() {
		case ugcstorev1.StructuredQuery_UnaryFilter_IS_NULL:
			return v == nil || v.GetNullValue() == 0 && v.GetValueType() == nil
		default:
			return true
		}
	}
	return true
}

func matchFieldFilter(f *ugcstorev1.StructuredQuery_FieldFilter, fields *commonpb.MapValue) bool {
	have := fields.GetFields()[f.GetField().GetFieldPath()]
	want := f.GetValue()
	switch f.GetOp() {
	case ugcstorev1.StructuredQuery_FieldFilter_EQUAL:
		return valuesEqual(have, want)
	case ugcstorev1.StructuredQuery_FieldFilter_NOT_EQUAL:
		return !valuesEqual(have, want)
	case ugcstorev1.StructuredQuery_FieldFilter_LESS_THAN:
		return compareValues(have, want) < 0
	case ugcstorev1.StructuredQuery_FieldFilter_LESS_THAN_OR_EQUAL:
		return compareValues(have, want) <= 0
	case ugcstorev1.StructuredQuery_FieldFilter_GREATER_THAN:
		return compareValues(have, want) > 0
	case ugcstorev1.StructuredQuery_FieldFilter_GREATER_THAN_OR_EQUAL:
		return compareValues(have, want) >= 0
	case ugcstorev1.StructuredQuery_FieldFilter_IN:
		for _, v := range want.GetArrayValue().GetValues() {
			if valuesEqual(have, v) {
				return true
			}
		}
		return false
	case ugcstorev1.StructuredQuery_FieldFilter_NOT_IN:
		for _, v := range want.GetArrayValue().GetValues() {
			if valuesEqual(have, v) {
				return false
			}
		}
		return true
	case ugcstorev1.StructuredQuery_FieldFilter_ARRAY_CONTAINS:
		for _, v := range have.GetArrayValue().GetValues() {
			if valuesEqual(v, want) {
				return true
			}
		}
		return false
	case ugcstorev1.StructuredQuery_FieldFilter_ARRAY_CONTAINS_ANY:
		for _, v := range have.GetArrayValue().GetValues() {
			for _, w := range want.GetArrayValue().GetValues() {
				if valuesEqual(v, w) {
					return true
				}
			}
		}
		return false
	}
	return true
}

// sortDocuments orders query results.
func sortDocuments(docs []DocumentRecord, orders []*ugcstorev1.StructuredQuery_Order) {
	sort.SliceStable(docs, func(i, j int) bool {
		fi := unmarshalMap(docs[i].Fields)
		fj := unmarshalMap(docs[j].Fields)
		for _, o := range orders {
			path := o.GetField().GetFieldPath()
			c := compareValues(fi.GetFields()[path], fj.GetFields()[path])
			if c == 0 {
				continue
			}
			if o.GetDirection() == ugcstorev1.StructuredQuery_Order_DESCENDING {
				return c > 0
			}
			return c < 0
		}
		// Newest first by default: every "recent content" list wants that.
		return docs[i].UpdatedAt.After(docs[j].UpdatedAt)
	})
}

// mergeFields applies only the masked paths of an update onto the existing fields.
func mergeFields(existing, update *commonpb.MapValue, paths []string) *commonpb.MapValue {
	out := &commonpb.MapValue{Fields: map[string]*commonpb.Value{}}
	for k, v := range existing.GetFields() {
		out.Fields[k] = v
	}
	for _, p := range paths {
		if v, ok := update.GetFields()[p]; ok {
			out.Fields[p] = v
		} else {
			delete(out.Fields, p)
		}
	}
	return out
}

// valuesEqual / compareValues implement the comparisons a query needs.
func valuesEqual(a, b *commonpb.Value) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.String() == b.String()
}

// compareValues orders two values, comparing numbers numerically, strings
// lexicographically and timestamps chronologically. Mismatched kinds compare
// equal, so a malformed query degrades to "no ordering" instead of nonsense.
func compareValues(a, b *commonpb.Value) int {
	if a == nil || b == nil {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			return -1
		default:
			return 1
		}
	}
	if af, aok := numeric(a); aok {
		if bf, bok := numeric(b); bok {
			switch {
			case af < bf:
				return -1
			case af > bf:
				return 1
			default:
				return 0
			}
		}
	}
	if as, aok := a.GetValueType().(*commonpb.Value_StringValue); aok {
		if bs, bok := b.GetValueType().(*commonpb.Value_StringValue); bok {
			return strings.Compare(as.StringValue, bs.StringValue)
		}
	}
	if at, aok := a.GetValueType().(*commonpb.Value_TimestampValue); aok {
		if bt, bok := b.GetValueType().(*commonpb.Value_TimestampValue); bok {
			ai, bi := at.TimestampValue.AsTime(), bt.TimestampValue.AsTime()
			switch {
			case ai.Before(bi):
				return -1
			case ai.After(bi):
				return 1
			default:
				return 0
			}
		}
	}
	return 0
}

// numeric extracts a number from a value, if it is one.
func numeric(v *commonpb.Value) (float64, bool) {
	switch body := v.GetValueType().(type) {
	case *commonpb.Value_IntegerValue:
		return float64(body.IntegerValue), true
	case *commonpb.Value_DoubleValue:
		return body.DoubleValue, true
	case *commonpb.Value_FloatValue:
		return float64(body.FloatValue), true
	}
	return 0, false
}

// addValues implements the increment transform.
func addValues(cur, delta *commonpb.Value) *commonpb.Value {
	c, _ := numeric(cur)
	d, _ := numeric(delta)
	sum := c + d
	// Keep integers integral: the game increments counters, and turning them
	// into floats would change how they serialise back.
	if isInt(cur) && isInt(delta) {
		return &commonpb.Value{ValueType: &commonpb.Value_IntegerValue{IntegerValue: int64(sum)}}
	}
	return &commonpb.Value{ValueType: &commonpb.Value_DoubleValue{DoubleValue: sum}}
}

// pickValue implements the maximum / minimum transforms.
func pickValue(cur, other *commonpb.Value, wantMax bool) *commonpb.Value {
	if cur == nil {
		return other
	}
	c := compareValues(cur, other)
	if (wantMax && c >= 0) || (!wantMax && c <= 0) {
		return cur
	}
	return other
}

func isInt(v *commonpb.Value) bool {
	if v == nil {
		return true
	}
	_, ok := v.GetValueType().(*commonpb.Value_IntegerValue)
	return ok
}

// marshalMap / unmarshalMap store a MapValue as protojson.
func marshalMap(m *commonpb.MapValue) string {
	if m == nil {
		return ""
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func unmarshalMap(s string) *commonpb.MapValue {
	if s == "" {
		return nil
	}
	var out commonpb.MapValue
	if err := protojson.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return &out
}

// aliasKey is the storage key of an alias.
func aliasKey(scope, code string) string {
	if scope == "" {
		return code
	}
	return scope + "-" + code
}

// randomToken mints an unguessable id (upload tokens and blob ids ARE the
// capability, so these must be random, not sequential).
func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// randomCode mints a player-typable content code.
func randomCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "AAAAAAAA"
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out)
}

// hashID names a blob by its content, so uploading the same replay twice does
// not store it twice.
func hashID(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// keys renders a set for a log line.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Browse returns the most recently updated documents of a scope (or of every
// scope when scope is empty), newest first, for the browse-style services.
func (s *Store) Browse(scope string, limit int) []*ugcstorev1.Document {
	if limit <= 0 {
		limit = 24
	}
	// Walk the aliases: they are what marks a document as PUBLISHED. A document
	// with no alias was written but never shared, and must not show up in a
	// browse.
	type entry struct {
		rec DocumentRecord
	}
	var found []DocumentRecord
	seen := map[string]bool{}
	s.aliases.Range(func(_ string, alias AliasRecord) bool {
		if scope != "" && alias.Scope != scope {
			return true
		}
		if seen[alias.Document] {
			return true
		}
		if rec, ok := s.docs.Get(alias.Document); ok {
			seen[alias.Document] = true
			found = append(found, rec)
		}
		return true
	})
	sort.Slice(found, func(i, j int) bool { return found[i].UpdatedAt.After(found[j].UpdatedAt) })
	if len(found) > limit {
		found = found[:limit]
	}
	out := make([]*ugcstorev1.Document, 0, len(found))
	for _, rec := range found {
		out = append(out, s.proto(rec))
	}
	return out
}
