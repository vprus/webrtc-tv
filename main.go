package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

const (
	roleViewer            = "viewer"
	roleCamera            = "camera"
	defaultLocalPort      = "8080"
	defaultTSNetHostname  = "tv"
	defaultTSNetHTTPSAddr = ":443"
	defaultVideoClockRate = 90000
	defaultVideoMTU       = 1200
	maxCameraSlots        = 3
)

//go:embed static/*
var staticFiles embed.FS

//go:embed media/testsrc.ivf
var demoVideo []byte

type server struct {
	video               videoSource
	mu                  sync.Mutex
	viewers             map[*viewerPeer]struct{}
	viewerSessions      map[string]*viewerPeer
	cameraSlots         [maxCameraSlots]*broadcasterPeer
	broadcasterSessions map[string]*broadcasterPeer
}

type videoSource struct {
	data          []byte
	mimeType      string
	frameDuration time.Duration
}

type offerRequest struct {
	Role         string         `json:"role"`
	SessionID    string         `json:"sessionId,omitempty"`
	OfferKind    string         `json:"offerKind,omitempty"`
	SelectedSlot int            `json:"selectedSlot,omitempty"`
	Type         webrtc.SDPType `json:"type"`
	SDP          string         `json:"sdp"`
}

const (
	offerKindInitial    = "initial"
	offerKindICERestart = "ice-restart"

	signalingCodeRestartRequiresReconnect = "restart_requires_full_reconnect"
	signalingCodeCameraSlotsFull          = "camera_slots_full"
	controlChannelLabel                   = "tv-control"
)

type appConfig struct {
	port          string
	useTSNet      bool
	tsnetHostname string
	tsnetStateDir string
	tsnetAuthKey  string
}

type viewerPeer struct {
	pc           *webrtc.PeerConnection
	sender       *webrtc.RTPSender
	outputTrack  *webrtc.TrackLocalStaticRTP
	outputCodec  string
	sessionID    string
	selectedSlot int

	mu                 sync.Mutex
	sourceMode         viewerSourceMode
	sourceCancel       context.CancelFunc
	continuity         viewerRTPContinuity
	broadcastSync      viewerBroadcastRewrite
	controlChannel     *webrtc.DataChannel
	controlChannelOpen bool
	negotiationMu      sync.Mutex
	controlSendMu      sync.Mutex
	cleanupOnce        sync.Once
}

type viewerSourceMode uint8

const (
	viewerSourceNone viewerSourceMode = iota
	viewerSourceDemo
	viewerSourceBroadcast
)

type viewerRTPContinuity struct {
	nextSequence     uint16
	nextTimestamp    uint32
	lastTimestampGap uint32
}

type viewerBroadcastRewrite struct {
	initialized      bool
	sourceBaseTS     uint32
	targetBaseTS     uint32
	lastSourceTS     uint32
	lastTimestampGap uint32
}

type viewerDemoPacketizer struct {
	packetizer      rtp.Packetizer
	timestamp       uint32
	samplesPerFrame uint32
}

type broadcasterPeer struct {
	pc        *webrtc.PeerConnection
	sessionID string
	slot      int

	relayCtx    context.Context
	relayCancel context.CancelFunc

	mu                 sync.RWMutex
	mimeType           string
	track              *webrtc.TrackRemote
	controlChannel     *webrtc.DataChannel
	controlChannelOpen bool
	lastRTPTimeStamp   uint32
	hasSeenTimestamp   bool
	framesReceived     uint64
	lastFrameAt        time.Time
	relayOnce          sync.Once
	negotiationMu      sync.Mutex
	controlSendMu      sync.Mutex
	cleanupOnce        sync.Once
}

type cameraAck struct {
	Type           string `json:"type"`
	FramesReceived uint64 `json:"framesReceived"`
	LastFrameAtMS  int64  `json:"lastFrameAtMs"`
}

type cameraSlotState struct {
	Slot     int  `json:"slot"`
	Occupied bool `json:"occupied"`
	Live     bool `json:"live"`
}

type channelStateMessage struct {
	Type         string            `json:"type"`
	Slots        []cameraSlotState `json:"slots"`
	SelectedSlot int               `json:"selectedSlot,omitempty"`
	AssignedSlot int               `json:"assignedSlot,omitempty"`
}

type viewerControlMessage struct {
	Type string `json:"type"`
	Slot int    `json:"slot,omitempty"`
}

func newServer() *server {
	video, err := loadVideoSource(demoVideo)
	if err != nil {
		panic(err)
	}

	return &server{
		video:               video,
		viewers:             make(map[*viewerPeer]struct{}),
		viewerSessions:      make(map[string]*viewerPeer),
		broadcasterSessions: make(map[string]*broadcasterPeer),
	}
}

func loadVideoSource(data []byte) (videoSource, error) {
	if len(data) == 0 {
		return videoSource{}, fmt.Errorf("embedded video is empty")
	}

	_, header, err := ivfreader.NewWith(bytes.NewReader(data))
	if err != nil {
		return videoSource{}, fmt.Errorf("read IVF header: %w", err)
	}

	mimeType, err := mimeTypeForFourCC(header.FourCC)
	if err != nil {
		return videoSource{}, err
	}

	// The IVF header is the source of truth for playback cadence, so the server
	// emits frames at the same rate the embedded clip was encoded with.
	frameDuration := time.Second * time.Duration(header.TimebaseNumerator) / time.Duration(header.TimebaseDenominator)
	if frameDuration <= 0 {
		return videoSource{}, fmt.Errorf(
			"invalid frame duration from IVF header: %d/%d",
			header.TimebaseNumerator,
			header.TimebaseDenominator,
		)
	}

	return videoSource{
		data:          data,
		mimeType:      mimeType,
		frameDuration: frameDuration,
	}, nil
}

func mimeTypeForFourCC(fourCC string) (string, error) {
	switch fourCC {
	case "VP80":
		return webrtc.MimeTypeVP8, nil
	case "VP90":
		return webrtc.MimeTypeVP9, nil
	case "AV01":
		return webrtc.MimeTypeAV1, nil
	default:
		return "", fmt.Errorf("unsupported IVF codec %q", fourCC)
	}
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}

	srv := newServer()
	if err := run(cfg, newMux(srv)); err != nil {
		log.Fatal(err)
	}
}

func parseConfig(args []string) (appConfig, error) {
	cfg := appConfig{
		port:          strings.TrimSpace(os.Getenv("PORT")),
		tsnetHostname: defaultTSNetHostname,
		tsnetAuthKey:  strings.TrimSpace(os.Getenv("TS_AUTHKEY")),
	}
	if cfg.port == "" {
		cfg.port = defaultLocalPort
	}

	fs := flag.NewFlagSet("tv", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.port, "port", cfg.port, "HTTP port or address for local mode")
	fs.BoolVar(&cfg.useTSNet, "tsnet", false, "serve over Tailscale userspace networking with HTTPS only")
	fs.StringVar(&cfg.tsnetHostname, "tsnet-hostname", cfg.tsnetHostname, "Tailscale device hostname")
	fs.StringVar(&cfg.tsnetStateDir, "tsnet-state-dir", "", "directory for persistent tsnet state")
	fs.StringVar(&cfg.tsnetAuthKey, "tsnet-authkey", cfg.tsnetAuthKey, "Tailscale auth key (defaults to TS_AUTHKEY)")
	if err := fs.Parse(args); err != nil {
		return appConfig{}, err
	}
	if fs.NArg() != 0 {
		return appConfig{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	cfg.port = strings.TrimSpace(cfg.port)
	if cfg.port == "" {
		return appConfig{}, fmt.Errorf("port must not be empty")
	}

	cfg.tsnetHostname = strings.TrimSpace(cfg.tsnetHostname)
	if cfg.useTSNet && cfg.tsnetHostname == "" {
		return appConfig{}, fmt.Errorf("tsnet hostname must not be empty")
	}

	cfg.tsnetStateDir = strings.TrimSpace(cfg.tsnetStateDir)
	if cfg.useTSNet && cfg.tsnetStateDir == "" {
		var err error
		cfg.tsnetStateDir, err = defaultTSNetStateDir()
		if err != nil {
			return appConfig{}, err
		}
	}

	cfg.tsnetAuthKey = strings.TrimSpace(cfg.tsnetAuthKey)
	return cfg, nil
}

func defaultTSNetStateDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}

	return filepath.Join(configDir, "tv", "tsnet"), nil
}

func run(cfg appConfig, handler http.Handler) error {
	if cfg.useTSNet {
		return serveTSNet(cfg, handler)
	}

	addr := localListenAddr(cfg.port)
	log.Printf("serving TV demo on http://localhost%s", addr)
	return http.ListenAndServe(addr, handler)
}

func localListenAddr(port string) string {
	if strings.Contains(port, ":") {
		return port
	}

	return ":" + port
}

func serveTSNet(cfg appConfig, handler http.Handler) error {
	if err := os.MkdirAll(cfg.tsnetStateDir, 0o700); err != nil {
		return fmt.Errorf("create tsnet state dir: %w", err)
	}

	ts := &tsnet.Server{
		Hostname: cfg.tsnetHostname,
		Dir:      cfg.tsnetStateDir,
		AuthKey:  cfg.tsnetAuthKey,
	}
	defer ts.Close()

	log.Printf("starting Tailscale userspace node %q with state in %s", cfg.tsnetHostname, cfg.tsnetStateDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go logTSNetStartupProgress(ctx, ts)

	status, err := ts.Up(ctx)
	if err != nil {
		return fmt.Errorf("bring tsnet node online: %w", err)
	}
	log.Printf("Tailscale node is running; tailnet IPs: %s", formatTailscaleIPs(status))

	ln, err := ts.ListenTLS("tcp", defaultTSNetHTTPSAddr)
	if err != nil {
		return fmt.Errorf("listen on tsnet https %s: %w", defaultTSNetHTTPSAddr, err)
	}
	defer ln.Close()

	log.Printf("serving TV demo on Tailscale %s", tsnetServeURL(status, cfg.tsnetHostname))
	return http.Serve(ln, handler)
}

func tsnetServeURL(status *ipnstate.Status, fallbackHostname string) string {
	if status != nil && len(status.CertDomains) > 0 {
		domains := status.CertDomains
		return "https://" + strings.TrimSuffix(domains[0], ".")
	}

	return "https://" + fallbackHostname
}

func logTSNetStartupProgress(ctx context.Context, ts *tsnet.Server) {
	lc, err := ts.LocalClient()
	if err != nil {
		log.Printf("tsnet local client unavailable: %v", err)
		return
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	lastMessage := ""
	for {
		status, err := lc.Status(ctx)
		if err == nil {
			message := formatTSNetStartupStatus(status)
			if message != "" && message != lastMessage {
				log.Printf("%s", message)
				lastMessage = message
			}

			if status.BackendState == "Running" {
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func formatTSNetStartupStatus(status *ipnstate.Status) string {
	if status == nil {
		return ""
	}

	switch status.BackendState {
	case "NeedsLogin", "NoState":
		if status.AuthURL != "" {
			return fmt.Sprintf("Tailscale login required; open: %s", status.AuthURL)
		}
		return "waiting for Tailscale login or approval..."
	case "NeedsMachineAuth":
		return "waiting for Tailscale machine approval..."
	case "Starting", "Stopped":
		return fmt.Sprintf("Tailscale backend state: %s", status.BackendState)
	case "Running":
		return "Tailscale backend state: Running"
	default:
		if status.BackendState == "" {
			return ""
		}
		return fmt.Sprintf("Tailscale backend state: %s", status.BackendState)
	}
}

func formatTailscaleIPs(status *ipnstate.Status) string {
	if status == nil || len(status.TailscaleIPs) == 0 {
		return "unassigned"
	}

	ips := make([]string, 0, len(status.TailscaleIPs))
	for _, ip := range status.TailscaleIPs {
		ips = append(ips, ip.String())
	}

	return strings.Join(ips, ", ")
}

func newMux(s *server) http.Handler {
	staticRoot, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/offer", s.handleOffer)
	mux.HandleFunc("/camera-slots", s.handleCameraSlots)
	mux.Handle("/", http.FileServerFS(staticRoot))
	return mux
}

func (s *server) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("%s only", http.MethodPost))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var request offerRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid session description")
		return
	}

	offer := webrtc.SessionDescription{Type: request.Type, SDP: request.SDP}
	if offer.Type != webrtc.SDPTypeOffer {
		writeError(w, http.StatusBadRequest, "expected an SDP offer")
		return
	}

	answer, err := s.createAnswer(request.Role, offer, request.SessionID, request.OfferKind, request.SelectedSlot)
	if err != nil {
		if errors.Is(err, errUnsupportedRole) || errors.Is(err, errUnsupportedOfferKind) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, errRestartRequiresReconnect) {
			writeErrorCode(w, http.StatusConflict, signalingCodeRestartRequiresReconnect, err.Error())
			return
		}
		if errors.Is(err, errCameraSlotsFull) {
			writeErrorCode(w, http.StatusConflict, signalingCodeCameraSlotsFull, err.Error())
			return
		}

		log.Printf("offer handling failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to create answer")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		log.Printf("failed to write answer: %v", err)
	}
}

var errUnsupportedRole = errors.New("unsupported WebRTC role")
var errUnsupportedOfferKind = errors.New("unsupported offer kind")
var errRestartRequiresReconnect = errors.New("restart requires full reconnect")
var errCameraSlotsFull = errors.New("camera slot limit reached")

func (s *server) createAnswer(role string, offer webrtc.SessionDescription, sessionID string, offerKind string, selectedSlot int) (webrtc.SessionDescription, error) {
	switch offerKind = normalizeOfferKind(offerKind); offerKind {
	case offerKindInitial, offerKindICERestart:
	default:
		return webrtc.SessionDescription{}, fmt.Errorf("%w %q", errUnsupportedOfferKind, offerKind)
	}

	switch normalizeRole(role) {
	case roleViewer:
		return s.createViewerAnswer(offer, sessionID, offerKind, selectedSlot)
	case roleCamera:
		return s.createCameraAnswer(offer, sessionID, offerKind)
	default:
		return webrtc.SessionDescription{}, fmt.Errorf("%w %q", errUnsupportedRole, role)
	}
}

func (s *server) handleCameraSlots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("%s only", http.MethodGet))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(channelStateMessage{
		Type:  "channelState",
		Slots: s.cameraSlotStates(),
	})
}

func normalizeRole(role string) string {
	role = strings.TrimSpace(strings.ToLower(role))
	if role == "" {
		return roleViewer
	}

	return role
}

func normalizeOfferKind(kind string) string {
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind == "" {
		return offerKindInitial
	}

	return kind
}

func normalizeSelectedSlot(slot int) int {
	if slot < 1 || slot > maxCameraSlots {
		return 1
	}

	return slot
}

func normalizeVideoMimeType(mimeType string) string {
	return strings.TrimSpace(strings.ToLower(mimeType))
}

func isSupportedBroadcastMimeType(mimeType string) bool {
	switch normalizeVideoMimeType(mimeType) {
	case strings.ToLower(webrtc.MimeTypeVP8), strings.ToLower(webrtc.MimeTypeH264):
		return true
	default:
		return false
	}
}

func newViewerOutputTrack(mimeType string) (*webrtc.TrackLocalStaticRTP, error) {
	mimeType = normalizeVideoMimeType(mimeType)
	if !isSupportedBroadcastMimeType(mimeType) {
		return nil, fmt.Errorf("unsupported viewer output codec %q", mimeType)
	}

	return webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: mimeType},
		"video",
		"tv",
	)
}

func initialViewerContinuity() viewerRTPContinuity {
	seed := uint32(time.Now().UnixNano())
	return viewerRTPContinuity{
		nextSequence:     uint16(seed),
		nextTimestamp:    seed,
		lastTimestampGap: defaultVideoClockRate / 30,
	}
}

func defaultTimestampGap(mimeType string) uint32 {
	_ = mimeType
	return defaultVideoClockRate / 30
}

func createPeerAnswer(pc *webrtc.PeerConnection, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	if err := pc.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, err
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, err
	}

	<-gatherComplete

	if pc.LocalDescription() == nil {
		return webrtc.SessionDescription{}, fmt.Errorf("local description missing after ICE gathering")
	}

	return *pc.LocalDescription(), nil
}

func (s *server) viewerForSession(sessionID string) *viewerPeer {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	viewer := s.viewerSessions[sessionID]
	if viewer == nil {
		return nil
	}

	if viewer.pc.SignalingState() == webrtc.SignalingStateClosed {
		delete(s.viewerSessions, sessionID)
		delete(s.viewers, viewer)
		return nil
	}

	return viewer
}

func (s *server) broadcasterForSession(sessionID string) *broadcasterPeer {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	broadcaster := s.broadcasterSessions[sessionID]
	if broadcaster == nil {
		return nil
	}

	if broadcaster.pc.SignalingState() == webrtc.SignalingStateClosed {
		delete(s.broadcasterSessions, sessionID)
		if broadcaster.slot >= 1 && broadcaster.slot <= maxCameraSlots && s.cameraSlots[broadcaster.slot-1] == broadcaster {
			s.cameraSlots[broadcaster.slot-1] = nil
		}
		return nil
	}

	return broadcaster
}

func (s *server) cameraSlotStates() []cameraSlotState {
	s.mu.Lock()
	defer s.mu.Unlock()

	slots := make([]cameraSlotState, 0, maxCameraSlots)
	for idx := range maxCameraSlots {
		broadcaster := s.cameraSlots[idx]
		live := broadcaster != nil && broadcaster.codec() != ""
		slots = append(slots, cameraSlotState{
			Slot:     idx + 1,
			Occupied: broadcaster != nil,
			Live:     live,
		})
	}

	return slots
}

func (s *server) nextAvailableCameraSlot() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	for idx := range maxCameraSlots {
		if s.cameraSlots[idx] == nil {
			return idx + 1
		}
	}

	return 0
}

func (s *server) slotBroadcaster(slot int) *broadcasterPeer {
	slot = normalizeSelectedSlot(slot)

	s.mu.Lock()
	defer s.mu.Unlock()

	broadcaster := s.cameraSlots[slot-1]
	if broadcaster == nil {
		return nil
	}
	if broadcaster.pc.SignalingState() == webrtc.SignalingStateClosed {
		s.cameraSlots[slot-1] = nil
		if broadcaster.sessionID != "" && s.broadcasterSessions[broadcaster.sessionID] == broadcaster {
			delete(s.broadcasterSessions, broadcaster.sessionID)
		}
		return nil
	}

	return broadcaster
}

func (s *server) slotBroadcasterCodec(slot int) string {
	broadcaster := s.slotBroadcaster(slot)
	if broadcaster == nil {
		return ""
	}

	return broadcaster.codec()
}

func (v *viewerPeer) createRestartAnswer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	v.negotiationMu.Lock()
	defer v.negotiationMu.Unlock()
	return createPeerAnswer(v.pc, offer)
}

func (b *broadcasterPeer) createRestartAnswer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	b.negotiationMu.Lock()
	defer b.negotiationMu.Unlock()
	return createPeerAnswer(b.pc, offer)
}

func (s *server) createViewerAnswer(offer webrtc.SessionDescription, sessionID string, offerKind string, selectedSlot int) (webrtc.SessionDescription, error) {
	selectedSlot = normalizeSelectedSlot(selectedSlot)

	if offerKind == offerKindICERestart {
		viewer := s.viewerForSession(sessionID)
		if viewer == nil {
			return webrtc.SessionDescription{}, errRestartRequiresReconnect
		}

		viewer.setSelectedSlot(selectedSlot)
		s.syncViewerSource(viewer)
		s.sendViewerChannelState(viewer)
		log.Printf("viewer %p renegotiating session %q", viewer.pc, sessionID)
		return viewer.createRestartAnswer(offer)
	}

	if viewer := s.viewerForSession(sessionID); viewer != nil {
		viewer.shutdown(s)
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return webrtc.SessionDescription{}, err
	}

	initialCodec := s.video.mimeType
	if broadcasterCodec := s.slotBroadcasterCodec(selectedSlot); broadcasterCodec != "" {
		initialCodec = broadcasterCodec
	}

	outputTrack, err := newViewerOutputTrack(initialCodec)
	if err != nil {
		_ = pc.Close()
		return webrtc.SessionDescription{}, err
	}

	sender, err := pc.AddTrack(outputTrack)
	if err != nil {
		_ = pc.Close()
		return webrtc.SessionDescription{}, err
	}

	viewer := &viewerPeer{
		pc:           pc,
		sender:       sender,
		outputTrack:  outputTrack,
		outputCodec:  normalizeVideoMimeType(initialCodec),
		sessionID:    strings.TrimSpace(sessionID),
		selectedSlot: selectedSlot,
		continuity:   initialViewerContinuity(),
	}

	s.registerViewer(viewer)
	go drainRTCP(sender)

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != controlChannelLabel {
			log.Printf("viewer %p opened unexpected data channel %q", pc, dc.Label())
			return
		}

		viewer.setControlChannel(dc)

		dc.OnOpen(func() {
			viewer.markControlChannelOpen(dc)
			s.sendViewerChannelState(viewer)
		})

		dc.OnClose(func() {
			viewer.clearControlChannel(dc)
		})

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			s.handleViewerControlMessage(viewer, msg)
		})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("viewer %p connection state: %s", pc, state.String())

		switch state {
		case webrtc.PeerConnectionStateConnected:
			if err := s.syncViewerSource(viewer); err != nil {
				log.Printf("viewer %p failed to start demo loop: %v", pc, err)
				viewer.shutdown(s)
				return
			}
			s.sendViewerChannelState(viewer)
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			viewer.shutdown(s)
		}
	})

	answer, err := createPeerAnswer(pc, offer)
	if err != nil {
		viewer.shutdown(s)
		return webrtc.SessionDescription{}, err
	}

	if err := s.syncViewerSource(viewer); err != nil {
		viewer.shutdown(s)
		return webrtc.SessionDescription{}, err
	}
	s.sendViewerChannelState(viewer)

	return answer, nil
}

func (s *server) createCameraAnswer(offer webrtc.SessionDescription, sessionID string, offerKind string) (webrtc.SessionDescription, error) {
	if offerKind == offerKindICERestart {
		broadcaster := s.broadcasterForSession(sessionID)
		if broadcaster == nil {
			return webrtc.SessionDescription{}, errRestartRequiresReconnect
		}

		log.Printf("camera %p renegotiating session %q", broadcaster.pc, sessionID)
		return broadcaster.createRestartAnswer(offer)
	}

	if broadcaster := s.broadcasterForSession(sessionID); broadcaster != nil {
		broadcaster.shutdown(s)
	}

	slot := s.nextAvailableCameraSlot()
	if slot == 0 {
		return webrtc.SessionDescription{}, errCameraSlotsFull
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return webrtc.SessionDescription{}, err
	}

	// From the server's perspective the browser camera peer only sends media to
	// us, so we negotiate a recvonly video transceiver and wait for OnTrack.
	if _, err := pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		_ = pc.Close()
		return webrtc.SessionDescription{}, err
	}

	relayCtx, relayCancel := context.WithCancel(context.Background())
	broadcaster := &broadcasterPeer{
		pc:          pc,
		sessionID:   strings.TrimSpace(sessionID),
		slot:        slot,
		relayCtx:    relayCtx,
		relayCancel: relayCancel,
	}

	s.mu.Lock()
	s.cameraSlots[slot-1] = broadcaster
	if broadcaster.sessionID != "" {
		s.broadcasterSessions[broadcaster.sessionID] = broadcaster
	}
	s.mu.Unlock()
	s.broadcastChannelState()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("camera %p connection state: %s", pc, state.String())

		switch state {
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			broadcaster.shutdown(s)
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != controlChannelLabel {
			log.Printf("camera %p opened unexpected data channel %q", pc, dc.Label())
			return
		}

		broadcaster.setControlChannel(dc)

		dc.OnOpen(func() {
			log.Printf("camera control channel %p open", pc)
			broadcaster.markControlChannelOpen(dc)
			if err := broadcaster.sendCameraAckSnapshot(); err != nil {
				log.Printf("failed to send initial camera ack: %v", err)
			}
			s.sendBroadcasterChannelState(broadcaster)
		})

		dc.OnClose(func() {
			log.Printf("camera control channel %p closed", pc)
			broadcaster.clearControlChannel(dc)
		})
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}

		mimeType := normalizeVideoMimeType(track.Codec().MimeType)
		if !isSupportedBroadcastMimeType(mimeType) {
			log.Printf("camera %p uses unsupported codec %q", pc, track.Codec().MimeType)
			broadcaster.shutdown(s)
			return
		}

		broadcaster.setTrack(track, mimeType)
		s.activateBroadcaster(broadcaster)

		// A browser connection can expose more than one remote track over time.
		// Relay exactly one track for the active broadcaster and let shutdown or a
		// replacement broadcaster reset the whole pipeline.
		broadcaster.relayOnce.Do(func() {
			go func() {
				if err := s.relayBroadcast(broadcaster, track); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
					log.Printf("camera relay %p stopped: %v", pc, err)
				}

				broadcaster.shutdown(s)
			}()
		})
	})

	answer, err := createPeerAnswer(pc, offer)
	if err != nil {
		broadcaster.shutdown(s)
		return webrtc.SessionDescription{}, err
	}

	s.sendBroadcasterChannelState(broadcaster)

	return answer, nil
}

func (s *server) relayBroadcast(broadcaster *broadcasterPeer, track *webrtc.TrackRemote) error {
	// Ask for a fresh keyframe right away, then keep requesting them so late
	// joiners and packet-loss recovery do not wait indefinitely for the next
	// natural keyframe from the camera.
	s.requestBroadcasterKeyFrame(broadcaster.slot)

	keyframeTicker := time.NewTicker(3 * time.Second)
	defer keyframeTicker.Stop()

	go func() {
		for {
			select {
			case <-broadcaster.relayCtx.Done():
				return
			case <-keyframeTicker.C:
				s.requestBroadcasterKeyFrame(broadcaster.slot)
			}
		}
	}()

	for {
		// Relay RTP packets verbatim into the codec-matched shared track. The
		// server does not transcode; viewers subscribe to the relay track that
		// matches the broadcaster's negotiated codec.
		packet, _, err := track.ReadRTP()
		if err != nil {
			if broadcaster.relayCtx.Err() != nil {
				return broadcaster.relayCtx.Err()
			}

			return err
		}

		if err := broadcaster.noteRTPTimeStamp(packet.Timestamp); err != nil {
			log.Printf("failed to send camera ack: %v", err)
		}

		for _, viewer := range s.snapshotViewers() {
			if err := viewer.writeBroadcastPacket(broadcaster.slot, packet); err != nil {
				log.Printf("viewer %p failed to forward live camera: %v", viewer.pc, err)
				viewer.shutdown(s)
			}
		}

		select {
		case <-broadcaster.relayCtx.Done():
			return broadcaster.relayCtx.Err()
		default:
		}
	}
}

func (s *server) registerViewer(viewer *viewerPeer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.viewers[viewer] = struct{}{}
	if viewer.sessionID != "" {
		s.viewerSessions[viewer.sessionID] = viewer
	}
}

func (s *server) unregisterViewer(viewer *viewerPeer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.viewers, viewer)
	if viewer.sessionID != "" && s.viewerSessions[viewer.sessionID] == viewer {
		delete(s.viewerSessions, viewer.sessionID)
	}
}

func (s *server) hasBroadcaster() bool {
	for _, slot := range s.cameraSlotStates() {
		if slot.Live {
			return true
		}
	}

	return false
}

func (s *server) activateBroadcaster(next *broadcasterPeer) {
	s.mu.Lock()
	s.cameraSlots[next.slot-1] = next
	if next.sessionID != "" {
		s.broadcasterSessions[next.sessionID] = next
	}
	s.mu.Unlock()

	log.Printf("camera %p is now live in slot %d with %s", next.pc, next.slot, next.codec())

	s.syncAllViewers()
	s.broadcastChannelState()
	s.requestBroadcasterKeyFrame(next.slot)
}

func (s *server) deactivateBroadcaster(candidate *broadcasterPeer) {
	s.mu.Lock()
	if candidate.sessionID != "" && s.broadcasterSessions[candidate.sessionID] == candidate {
		delete(s.broadcasterSessions, candidate.sessionID)
	}
	if candidate.slot >= 1 && candidate.slot <= maxCameraSlots && s.cameraSlots[candidate.slot-1] == candidate {
		s.cameraSlots[candidate.slot-1] = nil
	}
	s.mu.Unlock()

	log.Printf("camera %p in slot %d went offline", candidate.pc, candidate.slot)

	s.syncAllViewers()
	s.broadcastChannelState()
}

func (s *server) requestBroadcasterKeyFrame(slot int) {
	broadcaster := s.slotBroadcaster(slot)

	if broadcaster == nil {
		return
	}

	if err := broadcaster.requestKeyFrame(); err != nil {
		log.Printf("failed to request camera keyframe: %v", err)
	}
}

func (s *server) snapshotViewers() []*viewerPeer {
	s.mu.Lock()
	defer s.mu.Unlock()

	viewers := make([]*viewerPeer, 0, len(s.viewers))
	for viewer := range s.viewers {
		viewers = append(viewers, viewer)
	}

	return viewers
}

func (s *server) snapshotBroadcasters() []*broadcasterPeer {
	s.mu.Lock()
	defer s.mu.Unlock()

	broadcasters := make([]*broadcasterPeer, 0, maxCameraSlots)
	for _, broadcaster := range s.cameraSlots {
		if broadcaster != nil {
			broadcasters = append(broadcasters, broadcaster)
		}
	}

	return broadcasters
}

func (s *server) syncAllViewers() {
	for _, viewer := range s.snapshotViewers() {
		if err := s.syncViewerSource(viewer); err != nil {
			log.Printf("viewer %p failed to sync source: %v", viewer.pc, err)
			viewer.shutdown(s)
		}
		s.sendViewerChannelState(viewer)
	}
}

func (s *server) syncViewerSource(viewer *viewerPeer) error {
	slot := viewer.selectedChannel()
	if broadcasterCodec := s.slotBroadcasterCodec(slot); broadcasterCodec != "" {
		if err := viewer.useBroadcast(broadcasterCodec); err != nil {
			return err
		}
		s.requestBroadcasterKeyFrame(slot)
		return nil
	}

	return viewer.useDemo(s.video)
}

func (s *server) broadcastChannelState() {
	for _, viewer := range s.snapshotViewers() {
		s.sendViewerChannelState(viewer)
	}

	for _, broadcaster := range s.snapshotBroadcasters() {
		s.sendBroadcasterChannelState(broadcaster)
	}
}

func (s *server) sendViewerChannelState(viewer *viewerPeer) {
	_ = viewer.sendChannelState(channelStateMessage{
		Type:         "channelState",
		Slots:        s.cameraSlotStates(),
		SelectedSlot: viewer.selectedChannel(),
	})
}

func (s *server) sendBroadcasterChannelState(broadcaster *broadcasterPeer) {
	_ = broadcaster.sendChannelState(channelStateMessage{
		Type:         "channelState",
		Slots:        s.cameraSlotStates(),
		AssignedSlot: broadcaster.slot,
	})
}

func (s *server) handleViewerControlMessage(viewer *viewerPeer, msg webrtc.DataChannelMessage) {
	if !msg.IsString {
		return
	}

	var message viewerControlMessage
	if err := json.Unmarshal(msg.Data, &message); err != nil {
		return
	}

	if message.Type != "selectChannel" {
		return
	}

	viewer.setSelectedSlot(message.Slot)
	if err := s.syncViewerSource(viewer); err != nil {
		log.Printf("viewer %p failed to switch channel: %v", viewer.pc, err)
		viewer.shutdown(s)
		return
	}

	s.sendViewerChannelState(viewer)
}

func (v *viewerPeer) selectedChannel() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return normalizeSelectedSlot(v.selectedSlot)
}

func (v *viewerPeer) setSelectedSlot(slot int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.selectedSlot = normalizeSelectedSlot(slot)
}

func (v *viewerPeer) setControlChannel(dc *webrtc.DataChannel) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.controlChannel = dc
	v.controlChannelOpen = false
}

func (v *viewerPeer) markControlChannelOpen(dc *webrtc.DataChannel) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.controlChannel != dc {
		return
	}

	v.controlChannelOpen = true
}

func (v *viewerPeer) clearControlChannel(dc *webrtc.DataChannel) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.controlChannel != dc {
		return
	}

	v.controlChannel = nil
	v.controlChannelOpen = false
}

func (v *viewerPeer) sendChannelState(state channelStateMessage) error {
	v.mu.Lock()
	dc := v.controlChannel
	open := v.controlChannelOpen
	v.mu.Unlock()

	if dc == nil || !open {
		return nil
	}

	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}

	v.controlSendMu.Lock()
	defer v.controlSendMu.Unlock()
	return dc.SendText(string(payload))
}

func (v *viewerPeer) useBroadcast(mimeType string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if !isSupportedBroadcastMimeType(mimeType) {
		return fmt.Errorf("unsupported broadcast codec %q", mimeType)
	}

	if v.sourceCancel != nil {
		v.sourceCancel()
		v.sourceCancel = nil
	}

	if err := v.replaceOutputTrackLocked(mimeType); err != nil {
		return err
	}

	v.sourceMode = viewerSourceBroadcast
	v.broadcastSync = viewerBroadcastRewrite{
		lastTimestampGap: v.currentTimestampGapLocked(mimeType),
	}

	return nil
}

func (v *viewerPeer) useDemo(video videoSource) error {
	v.mu.Lock()

	if v.sourceCancel != nil {
		v.sourceCancel()
		v.sourceCancel = nil
	}

	if err := v.replaceOutputTrackLocked(video.mimeType); err != nil {
		v.mu.Unlock()
		return err
	}

	v.sourceMode = viewerSourceDemo
	if v.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		v.mu.Unlock()
		return nil
	}

	streamCtx, cancel := context.WithCancel(context.Background())
	v.sourceCancel = cancel
	initialSequence := v.continuity.nextSequence
	initialTimestamp := v.continuity.nextTimestamp
	track := v.outputTrack
	v.mu.Unlock()

	packetsPerFrame, err := newViewerDemoPacketizer(video, initialSequence, initialTimestamp)
	if err != nil {
		cancel()
		return err
	}

	go func() {
		if err := streamVideo(streamCtx, video, track, packetsPerFrame, v); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("viewer %p demo stream stopped: %v", v.pc, err)
		}
	}()

	return nil
}

func (v *viewerPeer) replaceOutputTrackLocked(mimeType string) error {
	mimeType = normalizeVideoMimeType(mimeType)
	if v.outputTrack != nil && v.outputCodec == mimeType {
		return nil
	}

	outputTrack, err := newViewerOutputTrack(mimeType)
	if err != nil {
		return err
	}

	if err := v.sender.ReplaceTrack(outputTrack); err != nil {
		return err
	}

	v.outputTrack = outputTrack
	v.outputCodec = mimeType
	return nil
}

func (v *viewerPeer) currentTimestampGapLocked(mimeType string) uint32 {
	if v.continuity.lastTimestampGap != 0 {
		return v.continuity.lastTimestampGap
	}

	return defaultTimestampGap(mimeType)
}

func (v *viewerPeer) writeBroadcastPacket(slot int, packet *rtp.Packet) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.sourceMode != viewerSourceBroadcast || v.outputTrack == nil || normalizeSelectedSlot(v.selectedSlot) != normalizeSelectedSlot(slot) {
		return nil
	}

	cloned := packet.Clone()
	if !v.broadcastSync.initialized {
		v.broadcastSync = viewerBroadcastRewrite{
			initialized:      true,
			sourceBaseTS:     cloned.Timestamp,
			targetBaseTS:     v.continuity.nextTimestamp,
			lastSourceTS:     cloned.Timestamp,
			lastTimestampGap: v.currentTimestampGapLocked(v.outputCodec),
		}
	}

	outTimestamp := v.broadcastSync.targetBaseTS + (cloned.Timestamp - v.broadcastSync.sourceBaseTS)
	if cloned.Timestamp != v.broadcastSync.lastSourceTS {
		step := cloned.Timestamp - v.broadcastSync.lastSourceTS
		if step == 0 {
			step = v.currentTimestampGapLocked(v.outputCodec)
		}
		v.broadcastSync.lastTimestampGap = step
		v.broadcastSync.lastSourceTS = cloned.Timestamp
	}

	cloned.SequenceNumber = v.continuity.nextSequence
	cloned.Timestamp = outTimestamp
	if err := v.outputTrack.WriteRTP(cloned); err != nil {
		return err
	}

	v.continuity.nextSequence++
	v.continuity.nextTimestamp = outTimestamp + v.broadcastSync.lastTimestampGap
	v.continuity.lastTimestampGap = v.broadcastSync.lastTimestampGap
	return nil
}

func (v *viewerPeer) writeDemoPackets(
	ctx context.Context,
	track *webrtc.TrackLocalStaticRTP,
	packets []*rtp.Packet,
	frameTimestamp uint32,
	nextTimestamp uint32,
) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if v.sourceMode != viewerSourceDemo || v.outputTrack != track {
		return context.Canceled
	}

	for _, packet := range packets {
		if err := track.WriteRTP(packet); err != nil {
			return err
		}
	}

	v.continuity.nextSequence = packets[len(packets)-1].SequenceNumber + 1
	v.continuity.nextTimestamp = nextTimestamp
	v.continuity.lastTimestampGap = nextTimestamp - frameTimestamp
	return nil
}

func (v *viewerPeer) shutdown(s *server) {
	v.cleanupOnce.Do(func() {
		v.mu.Lock()
		if v.sourceCancel != nil {
			v.sourceCancel()
			v.sourceCancel = nil
		}
		v.mu.Unlock()

		s.unregisterViewer(v)
		if v.pc.SignalingState() != webrtc.SignalingStateClosed {
			_ = v.pc.Close()
		}
	})
}

func (b *broadcasterPeer) setTrack(track *webrtc.TrackRemote, mimeType string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mimeType = normalizeVideoMimeType(mimeType)
	b.track = track
}

func (b *broadcasterPeer) setControlChannel(dc *webrtc.DataChannel) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.controlChannel = dc
	b.controlChannelOpen = false
}

func (b *broadcasterPeer) markControlChannelOpen(dc *webrtc.DataChannel) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.controlChannel != dc {
		return
	}

	b.controlChannelOpen = true
}

func (b *broadcasterPeer) clearControlChannel(dc *webrtc.DataChannel) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.controlChannel != dc {
		return
	}

	b.controlChannel = nil
	b.controlChannelOpen = false
}

func (b *broadcasterPeer) sendChannelState(state channelStateMessage) error {
	b.mu.RLock()
	dc := b.controlChannel
	open := b.controlChannelOpen
	b.mu.RUnlock()

	if dc == nil || !open {
		return nil
	}

	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}

	b.controlSendMu.Lock()
	defer b.controlSendMu.Unlock()
	return dc.SendText(string(payload))
}

func (b *broadcasterPeer) codec() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.mimeType
}

func (b *broadcasterPeer) noteRTPTimeStamp(timestamp uint32) error {
	b.mu.Lock()
	if b.hasSeenTimestamp && b.lastRTPTimeStamp == timestamp {
		b.mu.Unlock()
		return nil
	}

	b.hasSeenTimestamp = true
	b.lastRTPTimeStamp = timestamp
	b.framesReceived++
	b.lastFrameAt = time.Now()
	b.mu.Unlock()

	return b.sendCameraAckSnapshot()
}

func (b *broadcasterPeer) cameraAckSnapshot() (*webrtc.DataChannel, cameraAck, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.controlChannel == nil || !b.controlChannelOpen || b.lastFrameAt.IsZero() {
		return nil, cameraAck{}, false
	}

	return b.controlChannel, cameraAck{
		Type:           "cameraAck",
		FramesReceived: b.framesReceived,
		LastFrameAtMS:  b.lastFrameAt.UnixMilli(),
	}, true
}

func (b *broadcasterPeer) sendCameraAckSnapshot() error {
	dc, ack, ok := b.cameraAckSnapshot()
	if !ok {
		return nil
	}

	payload, err := json.Marshal(ack)
	if err != nil {
		return err
	}

	b.controlSendMu.Lock()
	defer b.controlSendMu.Unlock()
	return dc.SendText(string(payload))
}

func (b *broadcasterPeer) requestKeyFrame() error {
	b.mu.RLock()
	track := b.track
	b.mu.RUnlock()

	if track == nil {
		return nil
	}

	return b.pc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
	})
}

func (b *broadcasterPeer) shutdown(s *server) {
	b.cleanupOnce.Do(func() {
		b.relayCancel()
		s.deactivateBroadcaster(b)
		if b.pc.SignalingState() != webrtc.SignalingStateClosed {
			_ = b.pc.Close()
		}
	})
}

func streamVideo(
	ctx context.Context,
	video videoSource,
	track *webrtc.TrackLocalStaticRTP,
	packetizer *viewerDemoPacketizer,
	viewer *viewerPeer,
) error {
	for {
		// Reopen the IVF reader on EOF so the embedded clip loops forever without
		// carrying mutable read state between iterations.
		err := streamVideoLoop(ctx, video, track, packetizer, viewer)
		if errors.Is(err, io.EOF) {
			continue
		}

		return err
	}
}

func streamVideoLoop(
	ctx context.Context,
	video videoSource,
	track *webrtc.TrackLocalStaticRTP,
	packetizer *viewerDemoPacketizer,
	viewer *viewerPeer,
) error {
	reader, _, err := ivfreader.NewWith(bytes.NewReader(video.data))
	if err != nil {
		return err
	}

	// Pace sample writes to real time instead of dumping the whole file into the
	// peer connection as fast as possible.
	ticker := time.NewTicker(video.frameDuration)
	defer ticker.Stop()

	for {
		frame, _, err := reader.ParseNextFrame()
		if err != nil {
			return err
		}

		frameTimestamp := packetizer.timestamp
		packets := packetizer.packetizer.Packetize(frame, packetizer.samplesPerFrame)
		if len(packets) == 0 {
			continue
		}

		nextTimestamp := frameTimestamp + packetizer.samplesPerFrame
		if err := viewer.writeDemoPackets(ctx, track, packets, frameTimestamp, nextTimestamp); err != nil {
			return err
		}
		packetizer.timestamp = nextTimestamp

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func newViewerDemoPacketizer(video videoSource, initialSequence uint16, initialTimestamp uint32) (*viewerDemoPacketizer, error) {
	payloader, clockRate, err := payloaderForMimeType(video.mimeType)
	if err != nil {
		return nil, err
	}

	samplesPerFrame := uint32(video.frameDuration.Seconds() * float64(clockRate))
	if samplesPerFrame == 0 {
		return nil, fmt.Errorf("invalid RTP samples per frame for %s", video.mimeType)
	}

	return &viewerDemoPacketizer{
		packetizer: rtp.NewPacketizerWithOptions(
			defaultVideoMTU,
			payloader,
			rtp.NewFixedSequencer(initialSequence),
			clockRate,
			rtp.WithTimestamp(initialTimestamp),
		),
		timestamp:       initialTimestamp,
		samplesPerFrame: samplesPerFrame,
	}, nil
}

func payloaderForMimeType(mimeType string) (rtp.Payloader, uint32, error) {
	switch normalizeVideoMimeType(mimeType) {
	case strings.ToLower(webrtc.MimeTypeVP8):
		return &codecs.VP8Payloader{}, defaultVideoClockRate, nil
	case strings.ToLower(webrtc.MimeTypeH264):
		return &codecs.H264Payloader{}, defaultVideoClockRate, nil
	default:
		return nil, 0, fmt.Errorf("unsupported RTP payloader codec %q", mimeType)
	}
}

func drainRTCP(sender *webrtc.RTPSender) {
	buffer := make([]byte, 1500)
	for {
		// Even though the app ignores RTCP contents, Pion still expects the sender
		// side to drain feedback packets so interceptors and congestion control do
		// not stall.
		if _, _, err := sender.Read(buffer); err != nil {
			return
		}
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeErrorCode(w, status, "", message)
}

func writeErrorCode(w http.ResponseWriter, status int, code string, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	response := map[string]string{"error": message}
	if code != "" {
		response["code"] = code
	}
	_ = json.NewEncoder(w).Encode(response)
}
