package toyohr

import (
	"context"
	"log"
	"time"

	commonpb "github.com/NextendoNetwork/splatoon-3/gen/npln/common"
	toyohrv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/toyohr/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NextendoNetwork/splatoon-3/npln/names"
	"github.com/NextendoNetwork/splatoon-3/npln/store"
)

// GameRecordService implements nn.npln.toyohr.v1.GameRecord.
//
// This is where a player's ONLINE record lives: the Anarchy series challenge,
// the X-power measurement, the Salmon Run grade, the fest power, the "tag" a
// squad shares. The game asks the server to *initialise* these when a player
// first reaches a mode and then reads them back.
//
// # How much of this is understood
//
// InitializeAttributes is well described by the decompiled definition (it names
// every mode and the initial values the game proposes), and it expects an
// attribute map back. The rest of the service — the tag RPCs in particular —
// have "[UNKNOWN]" response bodies in the decompilation, which means nobody has
// documented their shape.
//
// So the implementation splits in two:
//
//	understood      InitializeAttributes, CreateBankaraChallenge,
//	                CreateFestPowerMeasurement: stored per player, echoed back in
//	                the documented shape.
//	not understood  the tag RPCs: answered with an empty success (their response
//	                messages ARE empty in the definition) and LOGGED with the
//	                request so their real shape can be filled in from a capture.
//
// That distinction is deliberate: a wrong guess in a record service silently
// corrupts a player's progression, while an empty success at worst leaves a
// feature inert.
type GameRecordService struct {
	names   names.Builder
	records *store.JSONMap[GameRecord]
}

// GameRecord is one player's stored online record.
type GameRecord struct {
	UserID string `json:"user_id"`
	PID    uint64 `json:"pid"`
	// Attributes is the protojson of the attribute map the game initialised.
	Attributes string `json:"attributes_json,omitempty"`
	// Season is the season the attributes were initialised for.
	Season string `json:"season,omitempty"`
	// Challenge / FestPower keep the last created records so a re-read after a
	// crash returns the same thing the game already saw.
	Challenge string    `json:"challenge_json,omitempty"`
	FestPower string    `json:"fest_power_json,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
}

// NewGameRecordService builds the service.
func NewGameRecordService(nb names.Builder, records *store.JSONMap[GameRecord]) *GameRecordService {
	return &GameRecordService{names: nb, records: records}
}

// InitializeAttributes sets up (or returns) a player's record for a mode.
//
// The request names an initialize_type and carries the initial values the game
// proposes for that mode (for instance InitialVs{season_id, initial_rate}). We
// store what the game asked for and echo the resulting attribute map back — the
// game is the authority on what those numbers mean, and it sent them.
func (s *GameRecordService) InitializeAttributes(ctx context.Context, req *toyohrv1.InitializeAttributesRequest) (*toyohrv1.InitializeAttributesResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	attrs := attributesFor(req)
	rec := s.records.Update(caller.UserID, func(cur GameRecord, found bool) GameRecord {
		if !found {
			cur = GameRecord{UserID: caller.UserID, PID: caller.PID, CreatedAt: now}
		}
		if attrs != nil {
			cur.Attributes = marshalMap(attrs)
		}
		if req.GetSeason() != "" {
			cur.Season = req.GetSeason()
		}
		cur.UpdatedAt = now
		return cur
	})
	log.Printf("[record] pid=%d InitializeAttributes type=%q season=%q -> %d attribute(s)",
		caller.PID, req.GetInitializeType(), req.GetSeason(), len(attrs.GetFields()))
	return &toyohrv1.InitializeAttributesResponse{
		Attributes: unmarshalMap(rec.Attributes),
		// document names the UGC document the record hangs off. Splatoon 3 uses
		// it to attach the record's detail blob; we point it at the player's own
		// record document so the name is stable and resolvable.
		Document: s.names.Document("gameRecords/" + caller.UserID),
	}, nil
}

// CreateBankaraChallenge records an Anarchy series challenge.
func (s *GameRecordService) CreateBankaraChallenge(ctx context.Context, req *toyohrv1.CreateBankaraChallengeRequest) (*toyohrv1.BankaraChallenge, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	fields := req.GetBankaraChallenge().GetFields()
	s.records.Update(caller.UserID, func(cur GameRecord, found bool) GameRecord {
		if !found {
			cur = GameRecord{UserID: caller.UserID, PID: caller.PID, CreatedAt: now}
		}
		cur.Challenge = marshalMap(fields)
		cur.UpdatedAt = now
		return cur
	})
	log.Printf("[record] pid=%d created an Anarchy challenge (%d field(s))", caller.PID, len(fields.GetFields()))
	return &toyohrv1.BankaraChallenge{
		Name:       s.names.User(caller.UserID) + "/bankaraChallenges/current",
		Fields:     fields,
		CreateTime: timestamppb.New(now),
		UpdateTime: timestamppb.New(now),
		Document:   s.names.Document("bankaraChallenges/" + caller.UserID),
	}, nil
}

// CreateFestPowerMeasurement records a fest-power measurement.
func (s *GameRecordService) CreateFestPowerMeasurement(ctx context.Context, req *toyohrv1.CreateFestPowerMeasurementRequest) (*toyohrv1.FestPowerMeasurement, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	fields := req.GetFestPowerMeasurement().GetFields()
	s.records.Update(caller.UserID, func(cur GameRecord, found bool) GameRecord {
		if !found {
			cur = GameRecord{UserID: caller.UserID, PID: caller.PID, CreatedAt: now}
		}
		cur.FestPower = marshalMap(fields)
		cur.UpdatedAt = now
		return cur
	})
	return &toyohrv1.FestPowerMeasurement{
		Name:       s.names.User(caller.UserID) + "/festPowerMeasurements/current",
		Fields:     fields,
		CreateTime: timestamppb.New(now),
		UpdateTime: timestamppb.New(now),
		Document:   s.names.Document("festPower/" + caller.UserID),
	}, nil
}

// InitializeTag sets up the shared "tag" a squad plays under.
//
// The response body is undocumented (empty in the decompiled definition), so we
// answer an empty success and log the request. That keeps the flow moving; if a
// capture ever shows a populated response, the log line below is where to start.
func (s *GameRecordService) InitializeTag(ctx context.Context, req *toyohrv1.InitializeTagRequest) (*toyohrv1.InitializeTagResponse, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	log.Printf("[record] pid=%d InitializeTag users=%d league=%q rule=%d (response shape undocumented -> empty success)",
		caller.PID, len(req.GetUsers()), req.GetLeagueSchedule(), req.GetGameRule())
	return &toyohrv1.InitializeTagResponse{}, nil
}

// GetTag returns a squad tag. Undocumented response; see InitializeTag.
func (s *GameRecordService) GetTag(ctx context.Context, req *toyohrv1.GetTagRequest) (*toyohrv1.GetTagResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	log.Printf("[record] GetTag %q (response shape undocumented -> empty success)", req.GetName())
	return &toyohrv1.GetTagResponse{}, nil
}

// SelectTags lists a player's tags. Undocumented response; see InitializeTag.
func (s *GameRecordService) SelectTags(ctx context.Context, req *toyohrv1.SelectTagsRequest) (*toyohrv1.SelectTagsResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	log.Printf("[record] SelectTags user=%q (response shape undocumented -> empty success)", req.GetUser())
	return &toyohrv1.SelectTagsResponse{}, nil
}

// attributesFor turns the mode-specific block of an InitializeAttributes request
// into the attribute map we store and echo back.
//
// Only the fields the game itself sent are reflected: the initial rate it
// proposes for a new season, the Salmon Run grade points it carries over, the
// season id. Nothing is invented, so a player's rank cannot be corrupted by a
// guess on our side.
func attributesFor(req *toyohrv1.InitializeAttributesRequest) *commonpb.MapValue {
	fields := map[string]*commonpb.Value{}
	set := func(key string, v *commonpb.Value) { fields[key] = v }

	if req.GetInitializeType() != "" {
		set("initialize_type", stringValue(req.GetInitializeType()))
	}
	if req.GetSeason() != "" {
		set("season", stringValue(req.GetSeason()))
	}
	if vs := req.GetInitialVs(); vs != nil {
		set("season_id", intValue(int64(vs.GetSeasonId())))
		set("initial_rate", doubleValue(vs.GetInitialRate()))
		set("transferred", boolValue(vs.GetTransferred()))
	}
	if shift := req.GetCoopShift(); shift != nil {
		set("coop_shift_id", stringValue(shift.GetShiftId()))
		set("coop_job_type", stringValue(shift.GetJobType()))
	}
	if grade := req.GetCoopGrade(); grade != nil {
		set("coop_total_grade_point", intValue(int64(grade.GetTotalGradePoint())))
	}
	if season := req.GetNewSeason(); season != nil {
		set("new_season_id", intValue(int64(season.GetSeasonId())))
	}
	if len(fields) == 0 {
		return nil
	}
	return &commonpb.MapValue{Fields: fields}
}

func stringValue(s string) *commonpb.Value {
	return &commonpb.Value{ValueType: &commonpb.Value_StringValue{StringValue: s}}
}

func intValue(n int64) *commonpb.Value {
	return &commonpb.Value{ValueType: &commonpb.Value_IntegerValue{IntegerValue: n}}
}

func doubleValue(f float64) *commonpb.Value {
	return &commonpb.Value{ValueType: &commonpb.Value_DoubleValue{DoubleValue: f}}
}

func boolValue(b bool) *commonpb.Value {
	return &commonpb.Value{ValueType: &commonpb.Value_BooleanValue{BooleanValue: b}}
}

// UserScreeningService implements nn.npln.toyohr.v1.UserScreening: player reports
// and the violations that follow them.
//
// Reports are STORED, not discarded. A community server needs a moderation trail,
// and nextendo-account already owns bans — so a report here is persisted with the
// reporter, the target and the reason, ready to be surfaced to an operator.
type UserScreeningService struct {
	names   names.Builder
	reports *store.JSONMap[ReportRecord]
}

// ReportRecord is one stored player report.
type ReportRecord struct {
	ID          string    `json:"id"`
	ReporterID  string    `json:"reporter_user_id"`
	ReporterPID uint64    `json:"reporter_pid"`
	Target      string    `json:"target"`
	TargetType  string    `json:"target_type"`
	Category    string    `json:"category"`
	Reason      string    `json:"reason"`
	Language    string    `json:"language_code"`
	CreatedAt   time.Time `json:"created_at"`
}

// NewUserScreeningService builds the service.
func NewUserScreeningService(nb names.Builder, reports *store.JSONMap[ReportRecord]) *UserScreeningService {
	return &UserScreeningService{names: nb, reports: reports}
}

// CreateReport stores a player report.
func (s *UserScreeningService) CreateReport(ctx context.Context, req *toyohrv1.CreateReportRequest) (*toyohrv1.Report, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	in := req.GetReport()
	now := time.Now().UTC()
	id := now.Format("20060102T150405") + "-" + caller.UserID
	rec := ReportRecord{
		ID:          id,
		ReporterID:  caller.UserID,
		ReporterPID: caller.PID,
		Target:      in.GetScreeningTarget(),
		TargetType:  in.GetScreeningTargetType(),
		Category:    in.GetCategory(),
		Reason:      in.GetReason(),
		Language:    in.GetLanguageCode(),
		CreatedAt:   now,
	}
	s.reports.Put(id, rec)
	log.Printf("[screening] pid=%d reported %q (%s / %s)", caller.PID, rec.Target, rec.Category, rec.Reason)
	return &toyohrv1.Report{
		Name:                s.names.Tenant() + "/reports/" + id,
		Category:            rec.Category,
		Reason:              rec.Reason,
		LanguageCode:        rec.Language,
		CreateTime:          timestamppb.New(now),
		ScreeningTarget:     rec.Target,
		ScreeningTargetType: rec.TargetType,
		Context:             in.GetContext(),
	}, nil
}

// GetViolation reports whether the player is under a moderation penalty.
//
// Bans live in nextendo-account and are enforced at login (the online gate), so a
// player who reaches this call is not banned: an empty violation is the accurate
// answer. The response shape is undocumented, so an empty message is also the
// safest one.
func (s *UserScreeningService) GetViolation(ctx context.Context, req *toyohrv1.GetViolationRequest) (*toyohrv1.GetViolationResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	return &toyohrv1.GetViolationResponse{}, nil
}

// SelectSnapshot returns the moderation snapshot of a piece of content.
func (s *UserScreeningService) SelectSnapshot(ctx context.Context, req *toyohrv1.SelectSnapshotRequest) (*toyohrv1.SelectSnapshotResponse, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	// Echo the value back: there is no screening pipeline on a private server, so
	// content is served as-is. Refusing here would block whatever the client was
	// about to show.
	return &toyohrv1.SelectSnapshotResponse{
		Name:        req.GetUser(),
		StringValue: req.GetStringValue(),
	}, nil
}
