package matchmaking

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	matchmakingv1 "github.com/n-popescu/splatoon-3/gen/npln/matchmaking/v1"
	"github.com/n-popescu/splatoon-3/internal/names"
)

// IceAllocator answers AllocateIceServerSet and ListLatencyMeasurementServers.
//
// # Why ICE and not the old NAT check
//
// The NEX titles in this stack (Mario Kart 8 Deluxe, Splatoon 2, Smash) punch
// holes with Pia's own NAT check: two probe servers at two distinct addresses
// (nextendo-nncs), then a direct UDP mesh. Splatoon 3 does it the modern way and
// asks NPLN for an ICE server set — a STUN server to discover its own address,
// and TURN servers to relay when a direct path cannot be found. So a Splatoon 3
// deployment needs a STUN/TURN server (coturn is the obvious choice) instead of
// the nncs pair.
//
// # Credentials
//
// TURN credentials are minted with coturn's REST-API scheme when a shared secret
// is configured:
//
//	username = "<unix expiry>:<npln user id>"
//	password = base64(HMAC-SHA1(secret, username))
//
// That way no static TURN credential is shipped in the client or in this
// repository, and a leaked one expires by itself. With no secret configured the
// static username/password from the environment is used, and with no TURN host at
// all only STUN is advertised (direct connections only — fine on open networks,
// and honest about it rather than pretending a relay exists).
type IceAllocator struct {
	names names.Builder

	stunHost string
	stunPort int

	turnHost     string
	turnPort     int
	turnSecret   string
	turnUser     string
	turnPassword string
	turnCredTTL  time.Duration

	ttl            time.Duration
	latencyServers []*matchmakingv1.LatencyMeasurementServer
}

// IceOptions configures the allocator.
type IceOptions struct {
	Names        names.Builder
	StunHost     string
	StunPort     int
	TurnHost     string
	TurnPort     int
	TurnSecret   string
	TurnUser     string
	TurnPassword string
	TurnCredTTL  time.Duration
	TTL          time.Duration
	// LatencyServers is the list ListLatencyMeasurementServers answers with.
	LatencyServers []LatencyServerConfig
}

// LatencyServerConfig is one latency-measurement endpoint from the config.
type LatencyServerConfig struct {
	Name     string
	Region   string
	Host     string
	Port     int
	Protocol string
}

// NewIceAllocator builds the allocator.
func NewIceAllocator(o IceOptions) *IceAllocator {
	if o.TurnCredTTL <= 0 {
		o.TurnCredTTL = time.Hour
	}
	if o.TTL <= 0 {
		o.TTL = 30 * time.Minute
	}
	a := &IceAllocator{
		names:        o.Names,
		stunHost:     o.StunHost,
		stunPort:     o.StunPort,
		turnHost:     o.TurnHost,
		turnPort:     o.TurnPort,
		turnSecret:   o.TurnSecret,
		turnUser:     o.TurnUser,
		turnPassword: o.TurnPassword,
		turnCredTTL:  o.TurnCredTTL,
		ttl:          o.TTL,
	}
	for _, ls := range o.LatencyServers {
		a.latencyServers = append(a.latencyServers, &matchmakingv1.LatencyMeasurementServer{
			Name:     a.names.LatencyMeasurementServer(ls.Name),
			Region:   ls.Region,
			Host:     ls.Host,
			Port:     int32(ls.Port),
			Protocol: latencyProtocol(ls.Protocol),
		})
	}
	return a
}

// Allocate returns the ICE server set for a user.
func (a *IceAllocator) Allocate(userID string) (*matchmakingv1.IceServerSet, error) {
	if a.stunHost == "" && a.turnHost == "" {
		// Answering an empty set would let the client believe NAT traversal is
		// configured and fail later, deep inside the P2P layer, with nothing in
		// the log. Failing here names the actual problem.
		return nil, fmt.Errorf("no STUN/TURN server is configured (set NPLN_STUN_HOST / NPLN_TURN_HOST)")
	}
	set := &matchmakingv1.IceServerSet{
		Name:                a.names.IceServerSet("default"),
		Ttl:                 durationpb.New(a.ttl),
		UpdateTime:          timestamppb.New(time.Now()),
		ClientCacheDuration: durationpb.New(a.ttl / 2),
	}
	if a.stunHost != "" {
		set.StunServer = &matchmakingv1.StunServer{
			Host:     a.stunHost,
			Port:     int32(a.stunPort),
			Protocol: matchmakingv1.StunServer_UDP,
		}
	}
	if a.turnHost != "" {
		user, pass := a.turnCredentials(userID)
		set.TurnServers = []*matchmakingv1.TurnServer{{
			Host:     a.turnHost,
			Port:     int32(a.turnPort),
			Protocol: matchmakingv1.TurnServer_UDP,
			Username: user,
			Password: pass,
		}}
	}
	return set, nil
}

// LatencyServers returns the configured latency-measurement servers.
//
// The slice is rebuilt per call (the protobuf messages themselves are shared and
// never mutated after construction), so a handler cannot reorder the list another
// request is reading.
func (a *IceAllocator) LatencyServers() []*matchmakingv1.LatencyMeasurementServer {
	out := make([]*matchmakingv1.LatencyMeasurementServer, len(a.latencyServers))
	copy(out, a.latencyServers)
	return out
}

// turnCredentials mints time-limited TURN credentials (coturn REST API scheme),
// or returns the static pair when no secret is configured.
func (a *IceAllocator) turnCredentials(userID string) (string, string) {
	if a.turnSecret == "" {
		return a.turnUser, a.turnPassword
	}
	expiry := time.Now().Add(a.turnCredTTL).Unix()
	username := strconv.FormatInt(expiry, 10) + ":" + userID
	mac := hmac.New(sha1.New, []byte(a.turnSecret))
	mac.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// latencyProtocol maps the config string onto the protobuf enum.
func latencyProtocol(s string) matchmakingv1.LatencyMeasurementServer_Protocol {
	switch s {
	case "tcp", "TCP":
		return matchmakingv1.LatencyMeasurementServer_TCP
	case "http", "HTTP":
		return matchmakingv1.LatencyMeasurementServer_HTTP
	default:
		return matchmakingv1.LatencyMeasurementServer_UDP
	}
}
