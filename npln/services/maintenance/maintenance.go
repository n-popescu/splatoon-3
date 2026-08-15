// Package maintenance implements nn.npln.maintenance.v1.MaintenanceScheduleService.
//
// The client subscribes at boot and keeps the stream open. Two things are served
// on it:
//
//	a schedule   when the operator has declared a maintenance window, the game
//	             shows its own maintenance screen and refuses to go online. That
//	             is far better than letting players walk into a half-restarted
//	             server and see a communication error.
//	keep-alives  otherwise, so the stream stays healthy and the game keeps
//	             believing the service is up.
//
// The window is read from the environment (NPLN_MAINTENANCE_START /
// NPLN_MAINTENANCE_END, RFC3339) and re-read on every subscribe, so declaring
// maintenance is a matter of setting the variables and restarting — no code
// change, and no need to take the server down to tell players it is going down.
package maintenance

import (
	"log"
	"os"
	"time"

	maintenancev1 "github.com/NextendoNetwork/splatoon-3/gen/npln/maintenance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NextendoNetwork/splatoon-3/npln/names"
)

// Service implements the maintenance service.
type Service struct {
	names names.Builder
	// keepAlive is how often an idle stream sends a keep-alive.
	keepAlive time.Duration
}

// New builds the service.
func New(nb names.Builder) *Service {
	return &Service{names: nb, keepAlive: 60 * time.Second}
}

// Window is a declared maintenance window.
type Window struct {
	Start time.Time
	End   time.Time
}

// CurrentWindow reads the configured window, or nil when none is declared.
func CurrentWindow() *Window {
	start, okStart := parseEnvTime("NPLN_MAINTENANCE_START")
	end, okEnd := parseEnvTime("NPLN_MAINTENANCE_END")
	if !okStart && !okEnd {
		return nil
	}
	if !okStart {
		start = time.Now()
	}
	if !okEnd {
		end = start.Add(2 * time.Hour)
	}
	return &Window{Start: start, End: end}
}

// ActiveReason returns a non-empty reason while maintenance is in progress. The
// gRPC interceptor uses it to answer every RPC with UNAVAILABLE_UNDER_MAINTENANCE.
func ActiveReason() string {
	w := CurrentWindow()
	if w == nil {
		return ""
	}
	now := time.Now()
	if now.Before(w.Start) || now.After(w.End) {
		return ""
	}
	return "scheduled maintenance until " + w.End.UTC().Format(time.RFC3339)
}

// SubscribeMaintenanceSchedules streams the maintenance schedule.
func (s *Service) SubscribeMaintenanceSchedules(req *maintenancev1.SubscribeMaintenanceSchedulesRequest, stream maintenancev1.MaintenanceScheduleService_SubscribeMaintenanceSchedulesServer) error {
	ctx := stream.Context()

	sendSchedule := func() error {
		w := CurrentWindow()
		if w == nil {
			return nil
		}
		log.Printf("[maintenance] announcing the window %s .. %s", w.Start.UTC().Format(time.RFC3339), w.End.UTC().Format(time.RFC3339))
		return stream.Send(&maintenancev1.SubscribeMaintenanceSchedulesResponse{
			Response: &maintenancev1.SubscribeMaintenanceSchedulesResponse_Message{
				Message: &maintenancev1.SubscribeMaintenanceSchedulesResponseMessage{
					Timestamp: timestamppb.New(time.Now()),
					Schedule: &maintenancev1.MaintenanceSchedule{
						Name:       s.names.Tenant() + "/maintenanceSchedules/current",
						StartTime:  timestamppb.New(w.Start),
						EndTime:    timestamppb.New(w.End),
						UpdateTime: timestamppb.New(time.Now()),
					},
				},
			},
		})
	}
	if err := sendSchedule(); err != nil {
		return err
	}

	t := time.NewTicker(s.keepAlive)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			// Re-check the window on every beat: an operator may declare one
			// while consoles are already connected, and this is how they hear
			// about it.
			if err := sendSchedule(); err != nil {
				return err
			}
			if err := stream.Send(&maintenancev1.SubscribeMaintenanceSchedulesResponse{
				Response: &maintenancev1.SubscribeMaintenanceSchedulesResponse_KeepAlive{
					KeepAlive: &maintenancev1.KeepAlive{},
				},
			}); err != nil {
				return err
			}
		}
	}
}

// parseEnvTime reads an RFC3339 time from the environment.
func parseEnvTime(key string) (time.Time, bool) {
	v := os.Getenv(key)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		log.Printf("[maintenance] %s=%q is not RFC3339; ignoring it", key, v)
		return time.Time{}, false
	}
	return t, true
}
