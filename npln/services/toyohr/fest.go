package toyohr

import (
	"context"
	"log"
	"time"

	toyohrv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/toyohr/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NextendoNetwork/splatoon-3/npln/names"
	"github.com/NextendoNetwork/splatoon-3/npln/nplnerr"
	"github.com/NextendoNetwork/splatoon-3/npln/server"
	"github.com/NextendoNetwork/splatoon-3/npln/store"
)

// FestService implements nn.npln.toyohr.v1.FestService — splatfests.
//
// A splatfest has three server-side parts:
//
//	the schedule    when it opens, starts, hits the halfway point, ends and
//	                closes, plus which content bundle the client downloads;
//	the entries     which team each player picked (stored per user, because the
//	                game asks for its own entry back on every boot);
//	the results     the ratios and points at the end, and the decryption keys
//	                that gate the announcement/result content.
//
// The schedule comes from schedule.json (the same file as the rotation, since a
// fest replaces the rotation while it runs). Results are configurable too, and
// default to "not valid yet", which is what the game shows while a fest is
// ongoing. That is honest: this server cannot compute real fest results without
// running the whole fest-power pipeline.
type FestService struct {
	names    names.Builder
	schedule *ScheduleService
	entries  *store.JSONMap[FestEntryRecord]
}

// FestEntryRecord is a player's team choice.
type FestEntryRecord struct {
	UserID    string    `json:"user_id"`
	PID       uint64    `json:"pid"`
	FestID    string    `json:"fest_id"`
	Team      string    `json:"team"`
	Region    string    `json:"region"`
	CreatedAt time.Time `json:"created_at"`
}

// NewFestService builds the service.
func NewFestService(nb names.Builder, schedule *ScheduleService, entries *store.JSONMap[FestEntryRecord]) *FestService {
	return &FestService{names: nb, schedule: schedule, entries: entries}
}

// SelectFestSchedule serves the current splatfest, if one is configured.
func (s *FestService) SelectFestSchedule(ctx context.Context, req *toyohrv1.SelectFestScheduleRequest) (*toyohrv1.SelectFestScheduleResponse, error) {
	fest := s.schedule.FestConfig()
	if fest == nil {
		// No fest is the normal state. An empty response (rather than NotFound)
		// is what tells the game "no splatfest right now" without an error
		// screen.
		return &toyohrv1.SelectFestScheduleResponse{}, nil
	}
	target := req.GetTarget()
	if target == "" {
		target = s.schedule.Target()
	}
	timetable, err := festTimetable(fest)
	if err != nil {
		return nil, nplnerr.Internal("the configured splatfest has an unreadable timetable: " + err.Error())
	}
	return &toyohrv1.SelectFestScheduleResponse{Schedule: &toyohrv1.FestSchedule{
		Name:                 s.names.FestSchedule(fest.ID),
		Fest:                 s.names.Fest(fest.ID),
		FestRegions:          fest.Regions,
		Target:               target,
		FestGameData:         fest.GameData,
		FestGameDataRevision: fest.GameDataRevision,
		Timetable:            timetable,
	}}, nil
}

// CreateFestEntry records the team a player picked.
func (s *FestService) CreateFestEntry(ctx context.Context, req *toyohrv1.CreateFestEntryRequest) (*toyohrv1.FestEntry, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	fest := s.schedule.FestConfig()
	if fest == nil {
		return nil, nplnerr.FailedPrecondition("no splatfest is running")
	}
	in := req.GetFestEntry()
	rec := FestEntryRecord{
		UserID:    caller.UserID,
		PID:       caller.PID,
		FestID:    fest.ID,
		Team:      in.GetFestTeam(),
		Region:    in.GetFestRegion(),
		CreatedAt: time.Now().UTC(),
	}
	// One entry per user per fest: joining a team is a one-time choice, and
	// letting it be rewritten would let a player switch team mid-fest.
	key := fest.ID + "/" + caller.UserID
	if existing, found := s.entries.Get(key); found {
		log.Printf("[fest] pid=%d already joined team %q; keeping it", caller.PID, existing.Team)
		return s.entryProto(existing), nil
	}
	s.entries.Put(key, rec)
	log.Printf("[fest] pid=%d joined team %q (region %q)", caller.PID, rec.Team, rec.Region)
	return s.entryProto(rec), nil
}

// GetFestEntry returns a player's team choice.
func (s *FestService) GetFestEntry(ctx context.Context, req *toyohrv1.GetFestEntryRequest) (*toyohrv1.FestEntry, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	fest := s.schedule.FestConfig()
	if fest == nil {
		return nil, nplnerr.NotFound("no splatfest is running")
	}
	rec, found := s.entries.Get(fest.ID + "/" + caller.UserID)
	if !found {
		return nil, nplnerr.NotFound("this player has not joined a team")
	}
	return s.entryProto(rec), nil
}

// GetFestResult returns the fest results.
//
// Defaults to "not valid": every result block carries an is_valid flag precisely
// so a client can be told "not yet". Reporting invented ratios would show players
// a fabricated outcome.
func (s *FestService) GetFestResult(ctx context.Context, req *toyohrv1.GetFestResultRequest) (*toyohrv1.FestResult, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	fest := s.schedule.FestConfig()
	if fest == nil {
		return nil, nplnerr.NotFound("no splatfest is running")
	}
	return &toyohrv1.FestResult{
		Name:          s.names.FestResult(fest.ID),
		Yobisai:       &toyohrv1.YobisaiResult{IsValid: false},
		HonsaiMidterm: &toyohrv1.HonsaiMidtermResult{IsValid: false},
		HonsaiFinal:   &toyohrv1.HonsaiFinalResult{IsValid: false},
		Overall:       &toyohrv1.OverallResult{IsValid: false},
	}, nil
}

// GetFestDecryptionKey serves the keys that unlock the fest content stages.
//
// The keys are content-gating, not security: the client downloads an encrypted
// bundle and the server releases the key when that part of the fest is due. With
// no key configured we return empty strings, which keeps the call answered (an
// unanswered call aborts the fest flow) while unlocking nothing.
func (s *FestService) GetFestDecryptionKey(ctx context.Context, req *toyohrv1.GetFestDecryptionKeyRequest) (*toyohrv1.FestDecryptionKey, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	fest := s.schedule.FestConfig()
	if fest == nil {
		return nil, nplnerr.NotFound("no splatfest is running")
	}
	return &toyohrv1.FestDecryptionKey{Name: s.names.FestDecryptionKey(fest.ID)}, nil
}

// entryProto renders a stored entry.
func (s *FestService) entryProto(rec FestEntryRecord) *toyohrv1.FestEntry {
	return &toyohrv1.FestEntry{
		Name:       s.names.FestEntry(rec.UserID),
		FestTeam:   rec.Team,
		FestRegion: rec.Region,
	}
}

// festTimetable converts the configured RFC3339 moments into the protobuf.
func festTimetable(f *FestConfig) (*toyohrv1.FestTimetable, error) {
	parse := func(s string) (*timestamppb.Timestamp, error) {
		if s == "" {
			return nil, nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, err
		}
		return timestamppb.New(t), nil
	}
	out := &toyohrv1.FestTimetable{}
	var err error
	if out.OpenTime, err = parse(f.OpenTime); err != nil {
		return nil, err
	}
	if out.StartTime, err = parse(f.StartTime); err != nil {
		return nil, err
	}
	if out.MidTime, err = parse(f.MidTime); err != nil {
		return nil, err
	}
	if out.EndTime, err = parse(f.EndTime); err != nil {
		return nil, err
	}
	if out.CloseTime, err = parse(f.CloseTime); err != nil {
		return nil, err
	}
	return out, nil
}

// requireCaller returns the authenticated, non-anonymous caller.
func requireCaller(ctx context.Context) (*server.Caller, error) {
	c, ok := server.CallerFrom(ctx)
	if !ok {
		return nil, nplnerr.TokenInvalid("no access token")
	}
	if c.Anonymous {
		return nil, nplnerr.PermissionDenied("the anonymous user cannot use the Splatoon 3 services")
	}
	if c.PID == 0 {
		return nil, nplnerr.InvalidAccount("this token carries no Nextendo account")
	}
	return c, nil
}
