package toyohr

import (
	"context"
	"log"
	"time"

	commonpb "github.com/NextendoNetwork/splatoon-3/gen/npln/common"
	toyohrv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/toyohr/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NextendoNetwork/splatoon-3/npln/names"
	"github.com/NextendoNetwork/splatoon-3/npln/nplnerr"
	"github.com/NextendoNetwork/splatoon-3/npln/store"
)

// CloudSaveService implements nn.npln.toyohr.v1.CloudSave.
//
// Splatoon 3 keeps a server-side "save record": a map of values the game writes
// as the player progresses, plus a name change RPC. It is not the console's save
// file — it is the online copy the game trusts for things it does not want a
// local save to own.
//
// # Persistence and ownership
//
// One record per NPLN user, stored as JSON under the data directory (the same
// shape nextendo-account uses for its own stores). A record is only ever readable
// and writable by its owner: the record name is derived from the caller's user
// id, and a request naming somebody else's record is refused rather than served,
// because this is where a player's progression lives.
type CloudSaveService struct {
	names   names.Builder
	records *store.JSONMap[SaveRecordRecord]
}

// SaveRecordRecord is one stored save record.
//
// SaveData is kept as the protojson form of the game's MapValue: it survives a
// restart, stays human-readable for support, and does not force us to understand
// a single one of the game's fields.
type SaveRecordRecord struct {
	UserID    string    `json:"user_id"`
	PID       uint64    `json:"pid"`
	SaveData  string    `json:"save_data_json"`
	UserName  string    `json:"user_name,omitempty"`
	Language  string    `json:"language_code,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewCloudSaveService builds the service.
func NewCloudSaveService(nb names.Builder, records *store.JSONMap[SaveRecordRecord]) *CloudSaveService {
	return &CloudSaveService{names: nb, records: records}
}

// CreateSaveRecord creates the caller's record.
func (s *CloudSaveService) CreateSaveRecord(ctx context.Context, req *toyohrv1.CreateSaveRecordRequest) (*toyohrv1.SaveRecord, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := s.records.Update(caller.UserID, func(cur SaveRecordRecord, found bool) SaveRecordRecord {
		if !found {
			cur = SaveRecordRecord{UserID: caller.UserID, PID: caller.PID, CreatedAt: now}
		}
		if data := req.GetSaveRecord().GetSaveData(); data != nil {
			cur.SaveData = marshalMap(data)
		}
		cur.UpdatedAt = now
		return cur
	})
	log.Printf("[cloudsave] created/refreshed record for pid=%d (%d bytes)", caller.PID, len(rec.SaveData))
	return s.proto(rec), nil
}

// GetSaveRecord returns the caller's record.
func (s *CloudSaveService) GetSaveRecord(ctx context.Context, req *toyohrv1.GetSaveRecordRequest) (*toyohrv1.SaveRecord, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	rec, found := s.records.Get(caller.UserID)
	if !found {
		// The game creates the record on first boot; NotFound is the expected
		// answer that triggers it.
		return nil, nplnerr.NotFound("no save record for this player yet")
	}
	return s.proto(rec), nil
}

// WriteSaveRecord applies a write to the caller's record.
//
// The request carries an update_time the client believes the record has. We use
// it as an optimistic-concurrency check: if the stored record is NEWER, the write
// is refused instead of overwriting progress the player made elsewhere (the
// "one place at a time" rule exists for the same reason).
func (s *CloudSaveService) WriteSaveRecord(ctx context.Context, req *toyohrv1.WriteSaveRecordRequest) (*toyohrv1.WriteSaveRecordResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var conflict bool
	s.records.Update(caller.UserID, func(cur SaveRecordRecord, found bool) SaveRecordRecord {
		if !found {
			cur = SaveRecordRecord{UserID: caller.UserID, PID: caller.PID, CreatedAt: now}
		}
		if ts := req.GetUpdateTime(); ts != nil && ts.IsValid() && !cur.UpdatedAt.IsZero() {
			if cur.UpdatedAt.After(ts.AsTime().UTC().Add(time.Second)) {
				conflict = true
				return cur
			}
		}
		if data := req.GetSaveRecord().GetSaveData(); data != nil {
			cur.SaveData = marshalMap(data)
		}
		cur.PID = caller.PID
		cur.UpdatedAt = now
		return cur
	})
	if conflict {
		return nil, nplnerr.FailedPrecondition("the stored save record is newer than the one being written")
	}
	log.Printf("[cloudsave] pid=%d wrote its record (event=%q)", caller.PID, req.GetSaveEventType())
	return &toyohrv1.WriteSaveRecordResponse{Timestamp: timestamppb.New(now)}, nil
}

// ValidateSaveRecord confirms the client's copy is current.
func (s *CloudSaveService) ValidateSaveRecord(ctx context.Context, req *toyohrv1.ValidateSaveRecordRequest) (*toyohrv1.ValidateSaveRecordResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	// The response body is not documented (the decompiled definition is empty),
	// so an empty success is both the accurate and the safe answer: it means
	// "validated" without inventing fields the client might parse.
	return &toyohrv1.ValidateSaveRecordResponse{}, nil
}

// ChangeUserName records the player name the game reports.
func (s *CloudSaveService) ChangeUserName(ctx context.Context, req *toyohrv1.ChangeUserNameRequest) (*toyohrv1.ChangeUserNameResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	s.records.Update(caller.UserID, func(cur SaveRecordRecord, found bool) SaveRecordRecord {
		if !found {
			cur = SaveRecordRecord{UserID: caller.UserID, PID: caller.PID, CreatedAt: now}
		}
		cur.UserName = req.GetUserName()
		cur.Language = string(req.GetLanguageCode())
		cur.UpdatedAt = now
		return cur
	})
	log.Printf("[cloudsave] pid=%d is now named %q", caller.PID, req.GetUserName())
	return &toyohrv1.ChangeUserNameResponse{
		Identifier: caller.UserID,
		Timestamp1: timestamppb.New(now),
		Timestamp2: timestamppb.New(now),
	}, nil
}

// DeleteSaveRecord removes the caller's record.
func (s *CloudSaveService) DeleteSaveRecord(ctx context.Context, req *toyohrv1.DeleteSaveRecordRequest) (*toyohrv1.DeleteSaveRecordResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	s.records.Delete(caller.UserID)
	log.Printf("[cloudsave] pid=%d deleted its record", caller.PID)
	return &toyohrv1.DeleteSaveRecordResponse{}, nil
}

// proto renders a stored record.
func (s *CloudSaveService) proto(rec SaveRecordRecord) *toyohrv1.SaveRecord {
	out := &toyohrv1.SaveRecord{
		Name:     s.names.SaveRecord(rec.UserID),
		SaveData: unmarshalMap(rec.SaveData),
	}
	if !rec.CreatedAt.IsZero() {
		out.CreateTime = timestamppb.New(rec.CreatedAt)
	}
	if !rec.UpdatedAt.IsZero() {
		out.UpdateTime = timestamppb.New(rec.UpdatedAt)
	}
	return out
}

// marshalMap stores a MapValue as protojson.
func marshalMap(m *commonpb.MapValue) string {
	if m == nil {
		return ""
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		log.Printf("[cloudsave] could not serialise a save map: %v", err)
		return ""
	}
	return string(b)
}

// unmarshalMap reads a MapValue back. A record written by an older build (or
// hand-edited badly) yields nil rather than taking the request down.
func unmarshalMap(s string) *commonpb.MapValue {
	if s == "" {
		return nil
	}
	var out commonpb.MapValue
	if err := protojson.Unmarshal([]byte(s), &out); err != nil {
		log.Printf("[cloudsave] stored save map is unreadable: %v", err)
		return nil
	}
	return &out
}
