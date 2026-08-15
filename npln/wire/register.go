// Package wire registers the service implementations on a gRPC server.
//
// The implementations live in internal/services/*; this package is the one place
// that knows which NPLN service each of them answers. It also holds the thin
// adapters needed where one implementation serves several NPLN services whose
// method names collide — Splatoon 3's Replay, Locker, Canola and CoopScenario all
// declare a method called RegisterDocument with a different request type, and Go
// cannot have four methods of that name on one type.
package wire

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	authv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/auth/v1"
	friendsv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/friends/v1"
	maintenancev1 "github.com/NextendoNetwork/splatoon-3/gen/npln/maintenance/v1"
	matchmakingv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/matchmaking/v1"
	messagingv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/messaging/v1"
	toyohrv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/toyohr/v1"
	ugcstorev1 "github.com/NextendoNetwork/splatoon-3/gen/npln/ugcstore/v1"

	authsvc "github.com/NextendoNetwork/splatoon-3/npln/services/auth"
	friendssvc "github.com/NextendoNetwork/splatoon-3/npln/services/friends"
	maintenancesvc "github.com/NextendoNetwork/splatoon-3/npln/services/maintenance"
	mmsvc "github.com/NextendoNetwork/splatoon-3/npln/services/matchmaking"
	msgsvc "github.com/NextendoNetwork/splatoon-3/npln/services/messaging"
	toyohrsvc "github.com/NextendoNetwork/splatoon-3/npln/services/toyohr"
	ugcsvc "github.com/NextendoNetwork/splatoon-3/npln/services/ugc"
)

// Services bundles every implementation.
type Services struct {
	Auth        *authsvc.Service
	Friends     *friendssvc.Service
	Presence    *friendssvc.PresenceService
	GameSession *mmsvc.GameSessionService
	Matchmaker  *mmsvc.MatchmakerService
	Messaging   *msgsvc.Service
	Maintenance *maintenancesvc.Service
	Schedule    *toyohrsvc.ScheduleService
	Fest        *toyohrsvc.FestService
	CloudSave   *toyohrsvc.CloudSaveService
	GameRecord  *toyohrsvc.GameRecordService
	Screening   *toyohrsvc.UserScreeningService
	Documents   *toyohrsvc.DocumentServices
	UGC         *ugcsvc.Service
}

// Register installs every service on the gRPC server.
func Register(srv *grpc.Server, s Services) {
	// General NPLN services.
	authv1.RegisterAuthServer(srv, s.Auth)
	authv1.RegisterUserServiceServer(srv, s.Auth)
	friendsv1.RegisterFriendsServer(srv, s.Friends)
	friendsv1.RegisterPresenceServiceServer(srv, s.Presence)
	matchmakingv1.RegisterGameSessionServiceServer(srv, s.GameSession)
	matchmakingv1.RegisterMatchmakerServer(srv, s.Matchmaker)
	messagingv1.RegisterMessagingServer(srv, s.Messaging)
	maintenancev1.RegisterMaintenanceScheduleServiceServer(srv, s.Maintenance)
	ugcstorev1.RegisterUgcstoreServer(srv, s.UGC)
	ugcstorev1.RegisterScreeningServer(srv, screeningAdapter{s.UGC})

	// Splatoon 3 ("toyohr") services.
	toyohrv1.RegisterScheduleServer(srv, s.Schedule)
	toyohrv1.RegisterFestServiceServer(srv, s.Fest)
	toyohrv1.RegisterCloudSaveServer(srv, s.CloudSave)
	toyohrv1.RegisterGameRecordServer(srv, s.GameRecord)
	toyohrv1.RegisterUserScreeningServer(srv, s.Screening)
	toyohrv1.RegisterLobbyMessagingServer(srv, lobbyAdapter{s.Messaging})
	toyohrv1.RegisterReplayServer(srv, replayAdapter{s.Documents})
	toyohrv1.RegisterLockerServer(srv, lockerAdapter{s.Documents})
	toyohrv1.RegisterCanolaServer(srv, canolaAdapter{s.Documents})
	toyohrv1.RegisterCoopScenarioServer(srv, coopScenarioAdapter{s.Documents})
}

// ---------------------------------------------------------------------------
// adapters
// ---------------------------------------------------------------------------

// lobbyAdapter maps LobbyMessaging's methods onto the messaging service, whose
// generic Messaging methods have the same names.
type lobbyAdapter struct{ s *msgsvc.Service }

func (a lobbyAdapter) RecvMessage(req *toyohrv1.RecvMessageRequest, stream toyohrv1.LobbyMessaging_RecvMessageServer) error {
	return a.s.RecvLobbyMessage(req, stream)
}

func (a lobbyAdapter) SendMessage(ctx context.Context, req *toyohrv1.SendMessageRequest) (*emptypb.Empty, error) {
	return a.s.SendLobbyDirect(ctx, req)
}

func (a lobbyAdapter) SendLobbyMessage(ctx context.Context, req *toyohrv1.SendLobbyMessageRequest) (*emptypb.Empty, error) {
	return a.s.SendLobbyMessage(ctx, req)
}

// replayAdapter exposes the replay half of the document services.
type replayAdapter struct{ d *toyohrsvc.DocumentServices }

func (a replayAdapter) RegisterDocument(ctx context.Context, req *toyohrv1.ReplayRegisterDocumentRequest) (*toyohrv1.ReplayRegisterDocumentResponse, error) {
	return a.d.ReplayRegisterDocument(ctx, req)
}

func (a replayAdapter) ResolveReplayCode(ctx context.Context, req *toyohrv1.ResolveReplayCodeRequest) (*toyohrv1.ResolveReplayCodeResponse, error) {
	return a.d.ResolveReplayCode(ctx, req)
}

func (a replayAdapter) FulfillReservations(ctx context.Context, req *toyohrv1.FulfillReservationsRequest) (*toyohrv1.FulfillReservationsResponse, error) {
	return a.d.FulfillReservations(ctx, req)
}

// lockerAdapter exposes the locker half.
type lockerAdapter struct{ d *toyohrsvc.DocumentServices }

func (a lockerAdapter) IkasuDocument(ctx context.Context, req *toyohrv1.IkasuDocumentRequest) (*toyohrv1.IkasuDocumentResponse, error) {
	return a.d.IkasuDocument(ctx, req)
}

func (a lockerAdapter) SelectDocuments(ctx context.Context, req *toyohrv1.SelectDocumentsRequest) (*toyohrv1.SelectDocumentsResponse, error) {
	return a.d.SelectDocuments(ctx, req)
}

func (a lockerAdapter) RegisterDocument(ctx context.Context, req *toyohrv1.LockerRegisterDocumentRequest) (*toyohrv1.LockerRegisterDocumentResponse, error) {
	return a.d.LockerRegisterDocument(ctx, req)
}

// canolaAdapter exposes the "ikasu post" half.
type canolaAdapter struct{ d *toyohrsvc.DocumentServices }

func (a canolaAdapter) IkasuDocument(ctx context.Context, req *toyohrv1.IkasuDocumentRequest) (*toyohrv1.IkasuDocumentResponse, error) {
	return a.d.IkasuDocument(ctx, req)
}

func (a canolaAdapter) SelectDocuments(ctx context.Context, req *toyohrv1.SelectDocumentsRequest) (*toyohrv1.SelectDocumentsResponse, error) {
	return a.d.SelectDocuments(ctx, req)
}

func (a canolaAdapter) RegisterDocument(ctx context.Context, req *toyohrv1.CanolaRegisterDocumentRequest) (*toyohrv1.CanolaRegisterDocumentResponse, error) {
	return a.d.CanolaRegisterDocument(ctx, req)
}

// coopScenarioAdapter exposes the Salmon Run scenario half.
type coopScenarioAdapter struct{ d *toyohrsvc.DocumentServices }

func (a coopScenarioAdapter) RegisterDocument(ctx context.Context, req *toyohrv1.CoopScenarioRegisterDocumentRequest) (*toyohrv1.CoopScenarioRegisterDocumentResponse, error) {
	return a.d.CoopScenarioRegisterDocument(ctx, req)
}

func (a coopScenarioAdapter) ResolveCoopScenarioCode(ctx context.Context, req *toyohrv1.ResolveCoopScenarioCodeRequest) (*toyohrv1.ResolveCoopScenarioCodeResponse, error) {
	return a.d.ResolveCoopScenarioCode(ctx, req)
}

// screeningAdapter exposes the UGC store's report method as the Screening
// service, which also serves an upload URI for report attachments.
type screeningAdapter struct{ s *ugcsvc.Service }

func (a screeningAdapter) CreateReport(ctx context.Context, req *ugcstorev1.CreateReportRequest) (*ugcstorev1.Report, error) {
	return a.s.CreateReport(ctx, req)
}

func (a screeningAdapter) IssueUploadUri(ctx context.Context, req *ugcstorev1.IssueUploadUriRequest) (*ugcstorev1.IssueUploadUriResponse, error) {
	return a.s.IssueUploadUri(ctx, req)
}
