package toyohr

import (
	"context"
	"os"
	"testing"
	"time"

	toyohrv1 "github.com/n-popescu/splatoon-3/gen/npln/toyohr/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/n-popescu/splatoon-3/internal/names"
)

func testSchedule(t *testing.T) *ScheduleService {
	t.Helper()
	return NewScheduleService(names.Builder{TenantID: "t-dce9377b-lp1"})
}

// TestVsSchedulesCoverNow: the game refuses to enter a mode whose rotation does
// not cover the present moment, so the first slot must contain "now".
func TestVsSchedulesCoverNow(t *testing.T) {
	s := testSchedule(t)
	now := time.Now().UTC()
	resp, err := s.SelectVsSchedules(context.Background(), &toyohrv1.SelectVsSchedulesRequest{
		Target:      "default",
		CurrentTime: timestamppb.New(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetSchedules()) == 0 {
		t.Fatal("no versus schedule was served")
	}
	first := resp.GetSchedules()[0]
	start, end := first.GetStartTime().AsTime(), first.GetEndTime().AsTime()
	if now.Before(start) || now.After(end) {
		t.Errorf("the first slot (%s .. %s) does not contain now (%s)", start, end, now)
	}
	if first.GetRegularSettings() == nil {
		t.Error("the slot has no regular-battle settings")
	}
	if len(first.GetBankaraSettings()) == 0 {
		t.Error("the slot has no Anarchy settings")
	}
}

// TestSlotsAreContiguousAndAligned: gaps or overlaps in the rotation make the game
// fall back to "no schedule", and a rotation that shifts on every restart makes it
// re-download constantly.
func TestSlotsAreContiguousAndAligned(t *testing.T) {
	s := testSchedule(t)
	resp, err := s.SelectVsSchedules(context.Background(), &toyohrv1.SelectVsSchedulesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	schedules := resp.GetSchedules()
	if len(schedules) < 3 {
		t.Fatalf("only %d slots were served", len(schedules))
	}
	for i := 1; i < len(schedules); i++ {
		prevEnd := schedules[i-1].GetEndTime().AsTime()
		start := schedules[i].GetStartTime().AsTime()
		if !prevEnd.Equal(start) {
			t.Fatalf("slot %d starts at %s but the previous one ends at %s", i, start, prevEnd)
		}
	}
	// Alignment: a two-hour rotation starts on an even hour, UTC, for everyone.
	start := schedules[0].GetStartTime().AsTime()
	if start.Minute() != 0 || start.Second() != 0 || start.Hour()%2 != 0 {
		t.Errorf("slots are not aligned to the rotation: first slot starts at %s", start)
	}
}

// TestSelectDurationExtendsTheAnswer: the client asks for enough schedule to cover
// a window, and must get it.
func TestSelectDurationExtendsTheAnswer(t *testing.T) {
	s := testSchedule(t)
	resp, err := s.SelectVsSchedules(context.Background(), &toyohrv1.SelectVsSchedulesRequest{
		SelectDuration: durationpb.New(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 24 hours of a 2-hour rotation is 12 slots (+1 for the current partial one).
	if got := len(resp.GetSchedules()); got < 12 {
		t.Errorf("served %d slots for a 24h window, want at least 12", got)
	}
}

// TestEtagSkipsTheDownload: answering the same etag with no entries is what saves
// the console from re-downloading an unchanged schedule.
func TestEtagSkipsTheDownload(t *testing.T) {
	s := testSchedule(t)
	first, err := s.SelectVsSchedules(context.Background(), &toyohrv1.SelectVsSchedulesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first.GetEtag() == "" {
		t.Fatal("no etag was served")
	}
	second, err := s.SelectVsSchedules(context.Background(), &toyohrv1.SelectVsSchedulesRequest{Etag: first.GetEtag()})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.GetSchedules()) != 0 {
		t.Error("the schedule was re-sent although the etag matched")
	}
	if second.GetEtag() != first.GetEtag() {
		t.Error("the etag changed between two identical requests")
	}
}

// TestCoopSchedules: Salmon Run needs a shift id per rotation, because results are
// reported against it.
func TestCoopSchedules(t *testing.T) {
	s := testSchedule(t)
	resp, err := s.SelectCoopSchedules(context.Background(), &toyohrv1.SelectCoopSchedulesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetSchedules()) == 0 {
		t.Fatal("no Salmon Run schedule was served")
	}
	first := resp.GetSchedules()[0]
	if first.GetShiftId() == "" {
		t.Error("the shift has no id")
	}
	if first.GetNormal() == nil {
		t.Error("the shift has no settings")
	}
}

// TestSeasonAlwaysOpen: ranked modes need a season that contains now, so the
// default must be a valid window rather than nothing.
func TestSeasonAlwaysOpen(t *testing.T) {
	s := testSchedule(t)
	resp, err := s.SelectSeasonSchedules(context.Background(), &toyohrv1.SelectSeasonSchedulesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetSchedules()) == 0 {
		t.Fatal("no season was served")
	}
	season := resp.GetSchedules()[0]
	now := time.Now().UTC()
	if now.Before(season.GetStartTime().AsTime()) || now.After(season.GetEndTime().AsTime()) {
		t.Errorf("the season (%s .. %s) does not contain now", season.GetStartTime().AsTime(), season.GetEndTime().AsTime())
	}
}

// TestNoFestByDefault: no splatfest is a normal state and must not be an error.
func TestNoFestByDefault(t *testing.T) {
	s := testSchedule(t)
	fest := NewFestService(names.Builder{TenantID: "t-dce9377b-lp1"}, s, nil)
	resp, err := fest.SelectFestSchedule(context.Background(), &toyohrv1.SelectFestScheduleRequest{})
	if err != nil {
		t.Fatalf("SelectFestSchedule with no fest configured returned an error: %v", err)
	}
	if resp.GetSchedule() != nil {
		t.Error("a splatfest was served although none is configured")
	}
}

// TestLoadFileOverridesTheDefaults checks the operator-facing configuration path.
func TestLoadFileOverridesTheDefaults(t *testing.T) {
	s := testSchedule(t)
	path := t.TempDir() + "/schedule.json"
	content := `{
	  "schedule_set_id": "test-set",
	  "target": "eu",
	  "rotation_minutes": 60,
	  "slot_count": 4,
	  "regular": [{"stages": [11, 12]}],
	  "bankara": [{"modes": [{"rule": 9, "stages": [13, 14]}]}],
	  "x": [{"rule": 8, "stages": [15, 16]}],
	  "coop": [{"stage": 21, "boss": "test-boss", "main_weapons": [1,2,3,4]}]
	}`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	if err := s.LoadFile(path); err != nil {
		t.Fatal(err)
	}
	resp, err := s.SelectVsSchedules(context.Background(), &toyohrv1.SelectVsSchedulesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.GetSchedules()); got != 4 {
		t.Errorf("served %d slots, want the configured 4", got)
	}
	first := resp.GetSchedules()[0]
	if got := first.GetRegularSettings().GetStages(); len(got) != 2 || got[0] != 11 {
		t.Errorf("regular stages = %v, want the configured [11 12]", got)
	}
	if got := first.GetBankaraSettings(); len(got) != 1 || got[0].GetRule() != 9 {
		t.Errorf("Anarchy settings = %v, want the configured rule 9", got)
	}
	// A one-hour rotation must align to the hour.
	if start := first.GetStartTime().AsTime(); start.Minute() != 0 {
		t.Errorf("a 60-minute rotation is not aligned to the hour: %s", start)
	}
	// A missing file must NOT wipe a loaded configuration.
	if err := s.LoadFile(t.TempDir() + "/absent.json"); err != nil {
		t.Fatalf("a missing schedule file must not be an error: %v", err)
	}
	if s.Target() != "eu" {
		t.Error("a missing file overwrote the loaded configuration")
	}
}

// writeFile is a tiny helper so the test reads top-down.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
