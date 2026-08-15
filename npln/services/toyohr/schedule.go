// Package toyohr implements the Splatoon-3-specific NPLN services.
//
// "toyohr" is Nintendo's internal codename for Splatoon 3, and every service in
// the nn.npln.toyohr.v1 package belongs to this one title:
//
//	Schedule       the rotation: which stages and rules are live, Salmon Run,
//	               seasons, event battles. Without it the online modes have
//	               nothing to show and refuse to start.
//	FestService    splatfests.
//	CloudSave      the save record the game syncs.
//	GameRecord     per-player online records (rank/x-power/Salmon Run grade).
//	Replay,
//	Locker,
//	Canola,
//	CoopScenario   user-generated content, addressed by short codes.
//	UserScreening  reporting / moderation.
//
// # What is real and what is configuration
//
// Nothing about Splatoon 3's *content* can be derived from the protocol: the
// stage numbers, rule numbers, weapon ids and season boundaries are game data.
// Retail Nintendo serves them from a schedule the game trusts blindly. So this
// package generates the schedule from an operator-editable file
// (schedule.example.json), with a deterministic rotation, instead of hardcoding
// values that would be wrong. See docs/SCHEDULE.md.
package toyohr

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	toyohrv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/toyohr/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NextendoNetwork/splatoon-3/npln/names"
)

// ScheduleConfig is the operator-editable description of the rotation.
type ScheduleConfig struct {
	// ScheduleSetID groups the entries the client caches together. Bump it when
	// you change the content, and the game will re-read the schedule.
	ScheduleSetID string `json:"schedule_set_id"`
	// Target is the schedule "target" the client asks for (a region/schedule
	// group). Requests naming another target still get this schedule, because a
	// private deployment has exactly one.
	Target string `json:"target"`
	// RotationMinutes is how long each slot lasts (retail is 120).
	RotationMinutes int `json:"rotation_minutes"`
	// SlotCount is how many future slots to serve in one answer.
	SlotCount int `json:"slot_count"`

	// Regular / Bankara (Anarchy) / X / League describe the versus rotation. The
	// stage and rule numbers are the game's own; they are passed through as-is.
	Regular []RegularSlot `json:"regular"`
	Bankara []BankaraSlot `json:"bankara"`
	X       []XSlot       `json:"x"`
	League  []LeagueSlot  `json:"league"`

	// Coop is the Salmon Run rotation.
	Coop []CoopSlot `json:"coop"`

	// Season describes the current season window.
	Season *SeasonConfig `json:"season"`

	// Fest, when set, describes the splatfest the FestService serves.
	Fest *FestConfig `json:"fest"`
}

// RegularSlot is one regular-battle rotation entry.
type RegularSlot struct {
	Stages []int32 `json:"stages"`
}

// BankaraSlot is one Anarchy entry (a rule plus its stages). Retail runs two
// concurrently (series and open), which is why the field is a list per slot.
type BankaraSlot struct {
	Modes []RuleStages `json:"modes"`
}

// XSlot is one X-battle entry.
type XSlot struct {
	Rule   int32   `json:"rule"`
	Stages []int32 `json:"stages"`
}

// LeagueSlot is one event-battle entry.
type LeagueSlot struct {
	Rule   int32   `json:"rule"`
	Stages []int32 `json:"stages"`
	// Slots are the sub-windows an event battle opens in (retail events run in
	// bursts rather than continuously).
	Slots []LeagueWindow `json:"slots"`
}

// LeagueWindow is one open window of an event battle, in minutes from the start
// of the entry.
type LeagueWindow struct {
	Name           string `json:"name"`
	StartOffsetMin int    `json:"start_offset_min"`
	LengthMin      int    `json:"length_min"`
}

// RuleStages pairs a rule number with its stages.
type RuleStages struct {
	Rule   int32   `json:"rule"`
	Stages []int32 `json:"stages"`
}

// CoopSlot is one Salmon Run rotation entry.
type CoopSlot struct {
	Stage       int32   `json:"stage"`
	Boss        string  `json:"boss"`
	MainWeapons []int64 `json:"main_weapons"`
	KumaWeapon  int32   `json:"kuma_weapon"`
	RewardType  string  `json:"reward_type"`
	RewardGear  int32   `json:"reward_gear_id"`
}

// SeasonConfig describes the current season.
type SeasonConfig struct {
	ID    string `json:"id"`
	Start string `json:"start"` // RFC3339
	End   string `json:"end"`   // RFC3339
}

// FestConfig describes a splatfest.
type FestConfig struct {
	ID      string   `json:"id"`
	Regions []string `json:"regions"`
	Teams   []string `json:"teams"`
	// The five timetable moments, RFC3339.
	OpenTime  string `json:"open_time"`
	StartTime string `json:"start_time"`
	MidTime   string `json:"mid_time"`
	EndTime   string `json:"end_time"`
	CloseTime string `json:"close_time"`
	// GameData / Revision name the fest content bundle the client downloads.
	GameData         string `json:"game_data"`
	GameDataRevision string `json:"game_data_revision"`
}

// ScheduleService implements nn.npln.toyohr.v1.Schedule.
type ScheduleService struct {
	names names.Builder

	mu   sync.RWMutex
	cfg  ScheduleConfig
	etag string
}

// NewScheduleService builds the service with a default (playable, minimal)
// configuration; call LoadFile to replace it.
func NewScheduleService(nb names.Builder) *ScheduleService {
	s := &ScheduleService{names: nb}
	s.setConfig(defaultScheduleConfig())
	return s
}

// LoadFile reads the schedule configuration. A missing file leaves the defaults
// in place and says so, because a schedule that silently became empty looks
// exactly like a broken game.
func (s *ScheduleService) LoadFile(path string) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[schedule] no schedule file at %s; serving the built-in placeholder rotation", path)
			return nil
		}
		return err
	}
	var cfg ScheduleConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("schedule: %s: %w", path, err)
	}
	s.setConfig(cfg)
	log.Printf("[schedule] loaded %s: set=%s rotation=%dmin regular=%d bankara=%d x=%d coop=%d fest=%v",
		path, cfg.ScheduleSetID, cfg.RotationMinutes, len(cfg.Regular), len(cfg.Bankara), len(cfg.X), len(cfg.Coop), cfg.Fest != nil)
	return nil
}

// setConfig normalises and installs a configuration.
func (s *ScheduleService) setConfig(cfg ScheduleConfig) {
	if cfg.RotationMinutes <= 0 {
		cfg.RotationMinutes = 120
	}
	if cfg.SlotCount <= 0 {
		cfg.SlotCount = 12
	}
	if cfg.Target == "" {
		cfg.Target = "default"
	}
	if cfg.ScheduleSetID == "" {
		cfg.ScheduleSetID = "nextendo-1"
	}
	s.mu.Lock()
	s.cfg = cfg
	// The etag lets the client skip a re-download. It must change when the
	// content changes and stay stable otherwise, so it is derived from the
	// schedule set id and the rotation length.
	s.etag = fmt.Sprintf("%s.%d", cfg.ScheduleSetID, cfg.RotationMinutes)
	s.mu.Unlock()
}

// FestConfig returns the configured splatfest, if any (used by FestService).
func (s *ScheduleService) FestConfig() *FestConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Fest
}

// Target returns the configured schedule target.
func (s *ScheduleService) Target() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Target
}

// SelectVsSchedules serves the versus rotation.
func (s *ScheduleService) SelectVsSchedules(ctx context.Context, req *toyohrv1.SelectVsSchedulesRequest) (*toyohrv1.SelectVsSchedulesResponse, error) {
	s.mu.RLock()
	cfg, etag := s.cfg, s.etag
	s.mu.RUnlock()

	now := requestTime(req.GetCurrentTime())
	slots := cfg.SlotCount
	// select_duration asks for "enough schedule to cover this long".
	if d := req.GetSelectDuration().AsDuration(); d > 0 {
		if n := int(d / (time.Duration(cfg.RotationMinutes) * time.Minute)); n > 0 {
			slots = n + 1
		}
	}
	target := req.GetTarget()
	if target == "" {
		target = cfg.Target
	}

	out := &toyohrv1.SelectVsSchedulesResponse{Etag: etag, Unk: true}
	// An unchanged etag means the client already has this schedule: answer with
	// the etag and no entries, which is what saves the console a download.
	if req.GetEtag() != "" && req.GetEtag() == etag {
		return out, nil
	}
	for i := 0; i < slots; i++ {
		start, end := slotWindow(now, cfg.RotationMinutes, i)
		idx := slotIndex(start, cfg.RotationMinutes)
		sched := &toyohrv1.VsSchedule{
			Name:          s.names.VsSchedule(target, fmt.Sprintf("%d", start.Unix())),
			StartTime:     timestamppb.New(start),
			EndTime:       timestamppb.New(end),
			ScheduleSetId: cfg.ScheduleSetID,
		}
		if len(cfg.Regular) > 0 {
			slot := cfg.Regular[idx%int64(len(cfg.Regular))]
			sched.RegularSettings = &toyohrv1.RegularSettings{Stages: slot.Stages}
		}
		for _, b := range pickBankara(cfg, idx) {
			sched.BankaraSettings = append(sched.BankaraSettings, &toyohrv1.BankaraSettings{
				Rule:   b.Rule,
				Stages: b.Stages,
			})
		}
		if len(cfg.X) > 0 {
			x := cfg.X[idx%int64(len(cfg.X))]
			sched.XSettings = &toyohrv1.XSettings{Rule: x.Rule, Stages: x.Stages}
		}
		out.Schedules = append(out.Schedules, sched)
	}
	return out, nil
}

// SelectVsParams serves the versus parameter set (tuning values the game reads
// alongside the rotation). We answer with whatever the configuration carries in
// its attributes, i.e. nothing by default — the game falls back to its built-in
// values, which is correct for a private deployment.
func (s *ScheduleService) SelectVsParams(ctx context.Context, req *toyohrv1.SelectVsParamsRequest) (*toyohrv1.SelectVsParamsResponse, error) {
	s.mu.RLock()
	cfg, etag := s.cfg, s.etag
	s.mu.RUnlock()
	target := req.GetTarget()
	if target == "" {
		target = cfg.Target
	}
	now := requestTime(req.GetCurrentTime())
	return &toyohrv1.SelectVsParamsResponse{
		Unk:  true,
		Etag: etag,
		Params: &toyohrv1.VsParams{
			Name:        s.names.VsParams(target, cfg.ScheduleSetID),
			Timestamp1:  timestamppb.New(now),
			Timestamp2:  timestamppb.New(now),
			ParamsSetId: cfg.ScheduleSetID,
		},
	}, nil
}

// SelectCoopSchedules serves the Salmon Run rotation.
func (s *ScheduleService) SelectCoopSchedules(ctx context.Context, req *toyohrv1.SelectCoopSchedulesRequest) (*toyohrv1.SelectCoopSchedulesResponse, error) {
	s.mu.RLock()
	cfg, etag := s.cfg, s.etag
	s.mu.RUnlock()

	out := &toyohrv1.SelectCoopSchedulesResponse{Etag: etag, Unk: true}
	if req.GetEtag() != "" && req.GetEtag() == etag {
		return out, nil
	}
	if len(cfg.Coop) == 0 {
		return out, nil
	}
	now := requestTime(req.GetCurrentTime())
	target := req.GetTarget()
	if target == "" {
		target = cfg.Target
	}
	for i := 0; i < cfg.SlotCount; i++ {
		start, end := slotWindow(now, cfg.RotationMinutes, i)
		idx := slotIndex(start, cfg.RotationMinutes)
		slot := cfg.Coop[idx%int64(len(cfg.Coop))]
		out.Schedules = append(out.Schedules, &toyohrv1.CoopSchedule{
			Name:          s.names.CoopSchedule(target, fmt.Sprintf("%d", start.Unix())),
			ScheduleSetId: cfg.ScheduleSetID,
			StartTime:     timestamppb.New(start),
			EndTime:       timestamppb.New(end),
			// The shift id identifies the rotation a result is reported against.
			ShiftId:   fmt.Sprintf("shift-%d", start.Unix()),
			Timestamp: timestamppb.New(start),
			Normal: &toyohrv1.CoopSchedule_Normal{
				Stage:        slot.Stage,
				Boss:         slot.Boss,
				MainWeapons:  slot.MainWeapons,
				KumaWeapon:   slot.KumaWeapon,
				RewardType:   slot.RewardType,
				RewardGearId: slot.RewardGear,
			},
		})
	}
	return out, nil
}

// SelectSeasonSchedules serves the season window.
func (s *ScheduleService) SelectSeasonSchedules(ctx context.Context, req *toyohrv1.SelectSeasonSchedulesRequest) (*toyohrv1.SelectSeasonSchedulesResponse, error) {
	s.mu.RLock()
	cfg, etag := s.cfg, s.etag
	s.mu.RUnlock()

	out := &toyohrv1.SelectSeasonSchedulesResponse{Etag: etag, Unk: true}
	if req.GetEtag() != "" && req.GetEtag() == etag {
		return out, nil
	}
	season := cfg.Season
	if season == nil {
		// No season configured: serve a rolling three-month window around now,
		// so ranked modes that require a season still open. A season the game
		// considers absent hides X battles entirely.
		now := time.Now().UTC()
		start := now.AddDate(0, -1, 0)
		end := now.AddDate(0, 2, 0)
		season = &SeasonConfig{ID: "current", Start: start.Format(time.RFC3339), End: end.Format(time.RFC3339)}
	}
	start, err1 := time.Parse(time.RFC3339, season.Start)
	end, err2 := time.Parse(time.RFC3339, season.End)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("schedule: season start/end must be RFC3339 (%v / %v)", err1, err2)
	}
	out.Schedules = append(out.Schedules, &toyohrv1.SeasonSchedule{
		Name:          s.names.SeasonSchedule(season.ID),
		StartTime:     timestamppb.New(start),
		EndTime:       timestamppb.New(end),
		ScheduleSetId: cfg.ScheduleSetID,
	})
	return out, nil
}

// SelectLeagueSchedules serves the event-battle schedule.
func (s *ScheduleService) SelectLeagueSchedules(ctx context.Context, req *toyohrv1.SelectLeagueSchedulesRequest) (*toyohrv1.SelectLeagueSchedulesResponse, error) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	out := &toyohrv1.SelectLeagueSchedulesResponse{}
	if len(cfg.League) == 0 {
		// No event configured is a normal state (retail has none most of the
		// time); an empty list is the right answer, not an error.
		return out, nil
	}
	now := requestTime(req.GetCurrentTime())
	for i, l := range cfg.League {
		start, end := slotWindow(now, cfg.RotationMinutes, i)
		entry := &toyohrv1.LeagueSchedule{
			Name:          s.names.LeagueSchedule(fmt.Sprintf("%s-%d", cfg.ScheduleSetID, i)),
			Rule:          l.Rule,
			Stages:        l.Stages,
			ScheduleSetId: cfg.ScheduleSetID,
			Timestamp:     timestamppb.New(start),
			StartTime:     timestamppb.New(start),
			EndTime:       timestamppb.New(end),
		}
		for _, w := range l.Slots {
			ws := start.Add(time.Duration(w.StartOffsetMin) * time.Minute)
			we := ws.Add(time.Duration(w.LengthMin) * time.Minute)
			entry.Slots = append(entry.Slots, &toyohrv1.LeagueSchedule_Slot{
				Name:      w.Name,
				StartTime: timestamppb.New(ws),
				EndTime:   timestamppb.New(we),
			})
		}
		out.Schedules = append(out.Schedules, entry)
	}
	return out, nil
}

// pickBankara returns the Anarchy entries for a slot.
func pickBankara(cfg ScheduleConfig, idx int64) []RuleStages {
	if len(cfg.Bankara) == 0 {
		return nil
	}
	return cfg.Bankara[idx%int64(len(cfg.Bankara))].Modes
}

// slotWindow returns the start and end of the n-th rotation slot from now.
//
// Slots are aligned to the rotation length since the Unix epoch, exactly like
// retail's on-the-hour rotations: every client computes the same boundaries, and
// a server restart does not shift them.
func slotWindow(now time.Time, rotationMinutes, n int) (time.Time, time.Time) {
	length := time.Duration(rotationMinutes) * time.Minute
	start := now.UTC().Truncate(length).Add(time.Duration(n) * length)
	return start, start.Add(length)
}

// slotIndex is the global index of a slot, used to walk the configured rotation
// deterministically.
func slotIndex(start time.Time, rotationMinutes int) int64 {
	length := int64(rotationMinutes) * 60
	if length <= 0 {
		length = 7200
	}
	idx := start.Unix() / length
	if idx < 0 {
		idx = -idx
	}
	return idx
}

// requestTime uses the client's notion of "now" when it sent one (it is the
// clock the schedule is compared against on the console), and ours otherwise.
func requestTime(ts *timestamppb.Timestamp) time.Time {
	if ts != nil && ts.IsValid() && ts.AsTime().Year() > 2000 {
		return ts.AsTime().UTC()
	}
	return time.Now().UTC()
}

// defaultScheduleConfig is the built-in placeholder rotation.
//
// It exists so a freshly-cloned server answers something coherent instead of an
// empty schedule (which reads as "online is broken"). The numbers are
// deliberately low-numbered stage/rule ids: they are placeholders to be replaced
// by a real schedule.json, and docs/SCHEDULE.md says so.
func defaultScheduleConfig() ScheduleConfig {
	return ScheduleConfig{
		ScheduleSetID:   "nextendo-placeholder-1",
		Target:          "default",
		RotationMinutes: 120,
		SlotCount:       12,
		Regular:         []RegularSlot{{Stages: []int32{1, 2}}, {Stages: []int32{3, 4}}},
		Bankara: []BankaraSlot{
			{Modes: []RuleStages{{Rule: 1, Stages: []int32{5, 6}}, {Rule: 2, Stages: []int32{7, 8}}}},
			{Modes: []RuleStages{{Rule: 3, Stages: []int32{9, 10}}, {Rule: 4, Stages: []int32{1, 3}}}},
		},
		X: []XSlot{{Rule: 1, Stages: []int32{2, 4}}, {Rule: 2, Stages: []int32{6, 8}}},
		Coop: []CoopSlot{{
			Stage:       1,
			MainWeapons: []int64{0, 1, 2, 3},
			RewardType:  "",
		}},
	}
}
