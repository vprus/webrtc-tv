package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"tailscale.com/ipn/ipnstate"
)

func TestRootServesHTML(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(newMux(newServer()))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}

	if !strings.Contains(string(body), "id=\"remoteVideo\"") {
		t.Fatalf("root page missing video element")
	}
}

func TestOfferRejectsWrongMethod(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/offer", nil)
	rec := httptest.NewRecorder()

	newMux(newServer()).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestOfferRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/offer", strings.NewReader("{not-json"))
	rec := httptest.NewRecorder()

	newMux(newServer()).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCreateAnswerRejectsUnsupportedRole(t *testing.T) {
	t.Parallel()

	_, err := newServer().createAnswer("projector", webrtc.SessionDescription{}, "", offerKindInitial, 1)
	if !errors.Is(err, errUnsupportedRole) {
		t.Fatalf("expected unsupported role error, got %v", err)
	}
}

func TestOfferRejectsUnknownRestartSession(t *testing.T) {
	server := newServer()
	client, _ := newViewerClient(t)
	defer client.Close()

	requestBody, err := json.Marshal(offerRequest{
		Role:      roleViewer,
		SessionID: "missing-viewer-session",
		OfferKind: offerKindICERestart,
		Type:      webrtc.SDPTypeOffer,
		SDP:       createLocalOffer(t, client).SDP,
	})
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/offer", bytes.NewReader(requestBody))
	rec := httptest.NewRecorder()

	newMux(server).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}

	var response map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if response["code"] != signalingCodeRestartRequiresReconnect {
		t.Fatalf("expected error code %q, got %q", signalingCodeRestartRequiresReconnect, response["code"])
	}
}

func TestOfferRejectsCameraWhenSlotsAreFull(t *testing.T) {
	server := newServer()
	cameras := make([]*webrtc.PeerConnection, 0, maxCameraSlots)
	defer func() {
		for _, camera := range cameras {
			_ = camera.Close()
		}
	}()

	for range maxCameraSlots {
		camera, _ := newCameraClient(t, webrtc.MimeTypeVP8)
		cameras = append(cameras, camera)

		answer, err := server.createAnswer(roleCamera, createLocalOffer(t, camera), "", offerKindInitial, 0)
		if err != nil {
			t.Fatalf("failed to allocate camera slot: %v", err)
		}

		if err := camera.SetRemoteDescription(answer); err != nil {
			t.Fatalf("failed to apply camera answer: %v", err)
		}
	}

	overflow, _ := newCameraClient(t, webrtc.MimeTypeVP8)
	defer overflow.Close()

	if _, err := server.createAnswer(roleCamera, createLocalOffer(t, overflow), "", offerKindInitial, 0); !errors.Is(err, errCameraSlotsFull) {
		t.Fatalf("expected camera slot limit error, got %v", err)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("TS_AUTHKEY", "")

	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}

	if cfg.useTSNet {
		t.Fatal("tsnet should be disabled by default")
	}
	if cfg.port != defaultLocalPort {
		t.Fatalf("expected default port %q, got %q", defaultLocalPort, cfg.port)
	}
	if cfg.tsnetHostname != defaultTSNetHostname {
		t.Fatalf("expected default tsnet hostname %q, got %q", defaultTSNetHostname, cfg.tsnetHostname)
	}
	if cfg.tsnetStateDir != "" {
		t.Fatalf("expected empty tsnet state dir in local mode, got %q", cfg.tsnetStateDir)
	}
}

func TestParseConfigTSNetDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PORT", "")
	t.Setenv("TS_AUTHKEY", "")

	expectedStateDir, err := defaultTSNetStateDir()
	if err != nil {
		t.Fatalf("defaultTSNetStateDir failed: %v", err)
	}

	cfg, err := parseConfig([]string{"-tsnet"})
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}

	if !cfg.useTSNet {
		t.Fatal("tsnet should be enabled")
	}
	if cfg.tsnetHostname != defaultTSNetHostname {
		t.Fatalf("expected tsnet hostname %q, got %q", defaultTSNetHostname, cfg.tsnetHostname)
	}
	if cfg.tsnetStateDir != expectedStateDir {
		t.Fatalf("expected tsnet state dir %q, got %q", expectedStateDir, cfg.tsnetStateDir)
	}
}

func TestParseConfigTSNetOverrides(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "from-env")

	cfg, err := parseConfig([]string{
		"-tsnet",
		"-tsnet-hostname", "living-room-tv",
		"-tsnet-state-dir", filepath.Join(t.TempDir(), "state"),
		"-tsnet-authkey", "tskey-test",
		"-port", "9090",
	})
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}

	if !cfg.useTSNet {
		t.Fatal("tsnet should be enabled")
	}
	if cfg.tsnetHostname != "living-room-tv" {
		t.Fatalf("expected overridden hostname, got %q", cfg.tsnetHostname)
	}
	if cfg.tsnetAuthKey != "tskey-test" {
		t.Fatalf("expected overridden auth key, got %q", cfg.tsnetAuthKey)
	}
	if !strings.Contains(cfg.tsnetStateDir, "state") {
		t.Fatalf("expected overridden state dir, got %q", cfg.tsnetStateDir)
	}
	if cfg.port != "9090" {
		t.Fatalf("expected local port override to be preserved, got %q", cfg.port)
	}
}

func TestLocalListenAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		port string
		want string
	}{
		{name: "port only", port: "8080", want: ":8080"},
		{name: "explicit wildcard addr", port: ":8080", want: ":8080"},
		{name: "explicit host addr", port: "127.0.0.1:8080", want: "127.0.0.1:8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := localListenAddr(tt.port); got != tt.want {
				t.Fatalf("localListenAddr(%q) = %q, want %q", tt.port, got, tt.want)
			}
		})
	}
}

func TestFormatTSNetStartupStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status *ipnstate.Status
		want   string
	}{
		{
			name: "needs login with auth url",
			status: &ipnstate.Status{
				BackendState: "NeedsLogin",
				AuthURL:      "https://login.tailscale.test/abc",
			},
			want: "Tailscale login required; open: https://login.tailscale.test/abc",
		},
		{
			name: "needs machine auth",
			status: &ipnstate.Status{
				BackendState: "NeedsMachineAuth",
			},
			want: "waiting for Tailscale machine approval...",
		},
		{
			name: "running",
			status: &ipnstate.Status{
				BackendState: "Running",
			},
			want: "Tailscale backend state: Running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatTSNetStartupStatus(tt.status); got != tt.want {
				t.Fatalf("formatTSNetStartupStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTSNetServeURL(t *testing.T) {
	t.Parallel()

	if got := tsnetServeURL(&ipnstate.Status{CertDomains: []string{"tv.example.ts.net"}}, "tv"); got != "https://tv.example.ts.net" {
		t.Fatalf("tsnetServeURL() with cert domain = %q", got)
	}

	if got := tsnetServeURL(nil, "tv"); got != "https://tv" {
		t.Fatalf("tsnetServeURL() fallback = %q", got)
	}
}

func TestFormatTailscaleIPs(t *testing.T) {
	t.Parallel()

	status := &ipnstate.Status{
		TailscaleIPs: []netip.Addr{
			netip.MustParseAddr("100.64.0.10"),
			netip.MustParseAddr("fd7a:115c:a1e0::10"),
		},
	}

	if got := formatTailscaleIPs(status); got != "100.64.0.10, fd7a:115c:a1e0::10" {
		t.Fatalf("formatTailscaleIPs() = %q", got)
	}
}

func TestCreateAnswerStreamsVideo(t *testing.T) {
	server := newServer()
	client, trackReady := newViewerClient(t)
	defer client.Close()

	answer, err := server.createAnswer(roleViewer, createLocalOffer(t, client), "", offerKindInitial, 1)
	if err != nil {
		t.Fatalf("failed to create viewer answer: %v", err)
	}

	if err := client.SetRemoteDescription(answer); err != nil {
		t.Fatalf("failed to set remote description: %v", err)
	}

	waitForPeerConnected(t, client, "viewer client")
	track := waitForRemoteVideoTrack(t, trackReady)
	waitForRTPPacket(t, track, "demo viewer")
}

func TestCreateAnswerReusesViewerSessionForICERestart(t *testing.T) {
	server := newServer()
	client, trackReady := newViewerClient(t)
	defer client.Close()

	const sessionID = "viewer-session"

	answer, err := server.createAnswer(roleViewer, createLocalOffer(t, client), sessionID, offerKindInitial, 1)
	if err != nil {
		t.Fatalf("failed to create initial viewer answer: %v", err)
	}

	if err := client.SetRemoteDescription(answer); err != nil {
		t.Fatalf("failed to set initial remote description: %v", err)
	}

	waitForPeerConnected(t, client, "viewer client")
	track := waitForRemoteVideoTrack(t, trackReady)
	waitForRTPPacket(t, track, "viewer before ICE restart")

	originalViewer := server.viewerForSession(sessionID)
	if originalViewer == nil {
		t.Fatal("expected viewer session to be registered")
	}

	restartAnswer, err := server.createAnswer(
		roleViewer,
		createLocalOfferWithOptions(t, client, &webrtc.OfferOptions{ICERestart: true}),
		sessionID,
		offerKindICERestart,
		1,
	)
	if err != nil {
		t.Fatalf("failed to create ICE restart answer: %v", err)
	}

	if err := client.SetRemoteDescription(restartAnswer); err != nil {
		t.Fatalf("failed to apply ICE restart answer: %v", err)
	}

	if reusedViewer := server.viewerForSession(sessionID); reusedViewer != originalViewer {
		t.Fatal("expected ICE restart to reuse the existing viewer peer")
	}

	waitForRTPPacket(t, track, "viewer after ICE restart")
}

func TestCreateAnswerReusesCameraSessionForICERestart(t *testing.T) {
	server := newServer()
	broadcaster, cameraTrack := newCameraClient(t, webrtc.MimeTypeVP8)
	defer broadcaster.Close()

	const sessionID = "camera-session"

	answer, err := server.createAnswer(roleCamera, createLocalOffer(t, broadcaster), sessionID, offerKindInitial, 0)
	if err != nil {
		t.Fatalf("failed to create initial camera answer: %v", err)
	}

	if err := broadcaster.SetRemoteDescription(answer); err != nil {
		t.Fatalf("failed to apply initial camera answer: %v", err)
	}

	waitForPeerConnected(t, broadcaster, "camera client")
	stopBroadcast := startBroadcastLoop(t, cameraTrack, firstVideoFrame(t, demoVideo))
	defer close(stopBroadcast)

	waitForCondition(t, "camera broadcaster to become live", 10*time.Second, server.hasBroadcaster)

	originalBroadcaster := server.broadcasterForSession(sessionID)
	if originalBroadcaster == nil {
		t.Fatal("expected camera session to be registered")
	}

	restartAnswer, err := server.createAnswer(
		roleCamera,
		createLocalOfferWithOptions(t, broadcaster, &webrtc.OfferOptions{ICERestart: true}),
		sessionID,
		offerKindICERestart,
		0,
	)
	if err != nil {
		t.Fatalf("failed to create camera ICE restart answer: %v", err)
	}

	if err := broadcaster.SetRemoteDescription(restartAnswer); err != nil {
		t.Fatalf("failed to apply camera ICE restart answer: %v", err)
	}

	if reusedBroadcaster := server.broadcasterForSession(sessionID); reusedBroadcaster != originalBroadcaster {
		t.Fatal("expected ICE restart to reuse the existing camera peer")
	}
}

func TestCameraControlChannelAcksReceivedFrames(t *testing.T) {
	server := newServer()
	broadcaster, cameraTrack, ackMessages := newCameraClientWithControlChannel(t, webrtc.MimeTypeVP8)
	defer broadcaster.Close()

	answer, err := server.createAnswer(roleCamera, createLocalOffer(t, broadcaster), "", offerKindInitial, 0)
	if err != nil {
		t.Fatalf("failed to create camera answer: %v", err)
	}

	if err := broadcaster.SetRemoteDescription(answer); err != nil {
		t.Fatalf("failed to apply camera answer: %v", err)
	}

	waitForPeerConnected(t, broadcaster, "camera client")

	stopBroadcast := startBroadcastLoop(t, cameraTrack, firstVideoFrame(t, demoVideo))
	defer close(stopBroadcast)

	waitForCondition(t, "camera broadcaster to become live", 10*time.Second, server.hasBroadcaster)

	var ack cameraAck
	waitForCondition(t, "camera ack message", 10*time.Second, func() bool {
		select {
		case message := <-ackMessages:
			if err := json.Unmarshal([]byte(message), &ack); err != nil {
				t.Fatalf("failed to decode camera ack: %v", err)
			}
			return ack.Type == "cameraAck" && ack.FramesReceived > 0 && ack.LastFrameAtMS > 0
		default:
			return false
		}
	})
}

func TestViewerKeepsReceivingPacketsAcrossBroadcastSwitches(t *testing.T) {
	server := newServer()

	viewer, trackReady := newViewerClient(t)
	defer viewer.Close()

	viewerAnswer, err := server.createAnswer(roleViewer, createLocalOffer(t, viewer), "", offerKindInitial, 1)
	if err != nil {
		t.Fatalf("failed to create viewer answer: %v", err)
	}

	if err := viewer.SetRemoteDescription(viewerAnswer); err != nil {
		t.Fatalf("failed to apply viewer answer: %v", err)
	}

	waitForPeerConnected(t, viewer, "viewer client")
	track := waitForRemoteVideoTrack(t, trackReady)
	waitForRTPPacket(t, track, "viewer demo before broadcast switch")

	broadcaster, cameraTrack := newCameraClient(t, webrtc.MimeTypeVP8)
	cameraAnswer, err := server.createAnswer(roleCamera, createLocalOffer(t, broadcaster), "", offerKindInitial, 0)
	if err != nil {
		_ = broadcaster.Close()
		t.Fatalf("failed to create camera answer: %v", err)
	}

	if err := broadcaster.SetRemoteDescription(cameraAnswer); err != nil {
		_ = broadcaster.Close()
		t.Fatalf("failed to apply camera answer: %v", err)
	}

	waitForPeerConnected(t, broadcaster, "camera client")
	stopBroadcast := startBroadcastLoop(t, cameraTrack, firstVideoFrame(t, demoVideo))

	waitForCondition(t, "camera broadcaster to become live", 10*time.Second, server.hasBroadcaster)
	time.Sleep(300 * time.Millisecond)
	waitForRTPPackets(t, track, 5, "viewer after camera switch")

	close(stopBroadcast)
	if err := broadcaster.Close(); err != nil {
		t.Fatalf("failed to close camera client: %v", err)
	}

	waitForCondition(t, "camera broadcaster to stop", 10*time.Second, func() bool { return !server.hasBroadcaster() })
	time.Sleep(300 * time.Millisecond)
	waitForRTPPackets(t, track, 5, "viewer after demo fallback")
}

func TestCameraBroadcastsToViewer(t *testing.T) {
	t.Run("VP8", func(t *testing.T) {
		testCameraBroadcastsToViewer(t, webrtc.MimeTypeVP8, firstVideoFrame(t, demoVideo))
	})

	t.Run("H264", func(t *testing.T) {
		testCameraBroadcastsToViewer(t, webrtc.MimeTypeH264, h264TestSample())
	})
}

func testCameraBroadcastsToViewer(t *testing.T, mimeType string, sample []byte) {
	server := newServer()

	broadcaster, cameraTrack := newCameraClient(t, mimeType)
	defer broadcaster.Close()

	cameraAnswer, err := server.createAnswer(roleCamera, createLocalOffer(t, broadcaster), "", offerKindInitial, 0)
	if err != nil {
		t.Fatalf("failed to create camera answer: %v", err)
	}

	if err := broadcaster.SetRemoteDescription(cameraAnswer); err != nil {
		t.Fatalf("failed to apply camera answer: %v", err)
	}

	waitForPeerConnected(t, broadcaster, "camera client")

	stopBroadcast := startBroadcastLoop(t, cameraTrack, sample)
	defer close(stopBroadcast)

	waitForCondition(t, "camera broadcaster to become live", 10*time.Second, server.hasBroadcaster)

	viewer, trackReady := newViewerClient(t)
	defer viewer.Close()

	viewerAnswer, err := server.createAnswer(roleViewer, createLocalOffer(t, viewer), "", offerKindInitial, 1)
	if err != nil {
		t.Fatalf("failed to create viewer answer: %v", err)
	}

	if err := viewer.SetRemoteDescription(viewerAnswer); err != nil {
		t.Fatalf("failed to apply viewer answer: %v", err)
	}

	waitForPeerConnected(t, viewer, "viewer client")
	track := waitForRemoteVideoTrack(t, trackReady)
	waitForRTPPacket(t, track, fmt.Sprintf("broadcast viewer (%s)", mimeType))
}

func newViewerClient(t *testing.T) (*webrtc.PeerConnection, chan *webrtc.TrackRemote) {
	t.Helper()

	client, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("failed to create viewer client peer connection: %v", err)
	}

	if _, err := client.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		_ = client.Close()
		t.Fatalf("failed to add viewer recvonly transceiver: %v", err)
	}

	trackReady := make(chan *webrtc.TrackRemote, 1)
	client.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			select {
			case trackReady <- track:
			default:
			}
		}
	})

	return client, trackReady
}

func newCameraClient(t *testing.T, mimeType string) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticSample) {
	t.Helper()

	client, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("failed to create camera peer connection: %v", err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: mimeType},
		"video",
		"camera",
	)
	if err != nil {
		_ = client.Close()
		t.Fatalf("failed to create local camera track: %v", err)
	}

	sender, err := client.AddTrack(track)
	if err != nil {
		_ = client.Close()
		t.Fatalf("failed to add local camera track: %v", err)
	}

	go drainRTCP(sender)

	return client, track
}

func newCameraClientWithControlChannel(t *testing.T, mimeType string) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticSample, chan string) {
	t.Helper()

	client, track := newCameraClient(t, mimeType)
	ackMessages := make(chan string, 16)

	dataChannel, err := client.CreateDataChannel(controlChannelLabel, nil)
	if err != nil {
		_ = client.Close()
		t.Fatalf("failed to create camera control data channel: %v", err)
	}

	dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}

		select {
		case ackMessages <- string(msg.Data):
		default:
		}
	})

	return client, track, ackMessages
}

func createLocalOffer(t *testing.T, pc *webrtc.PeerConnection) webrtc.SessionDescription {
	t.Helper()

	return createLocalOfferWithOptions(t, pc, nil)
}

func createLocalOfferWithOptions(t *testing.T, pc *webrtc.PeerConnection, options *webrtc.OfferOptions) webrtc.SessionDescription {
	t.Helper()

	offer, err := pc.CreateOffer(options)
	if err != nil {
		t.Fatalf("failed to create offer: %v", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("failed to set local description: %v", err)
	}
	<-gatherComplete

	if pc.LocalDescription() == nil {
		t.Fatal("local description missing after ICE gathering")
	}

	return *pc.LocalDescription()
}

func waitForPeerConnected(t *testing.T, pc *webrtc.PeerConnection, name string) {
	t.Helper()

	if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
		return
	}

	connected := make(chan struct{}, 1)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s to connect", name)
	}
}

func waitForRemoteVideoTrack(t *testing.T, trackReady chan *webrtc.TrackRemote) *webrtc.TrackRemote {
	t.Helper()

	select {
	case track := <-trackReady:
		return track
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for remote video track")
		return nil
	}
}

func waitForRTPPacket(t *testing.T, track *webrtc.TrackRemote, label string) {
	t.Helper()

	packetReceived := make(chan struct{})
	go func() {
		if _, _, err := track.ReadRTP(); err == nil {
			close(packetReceived)
		}
	}()

	select {
	case <-packetReceived:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for RTP packet for %s", label)
	}
}

func waitForRTPPackets(t *testing.T, track *webrtc.TrackRemote, count int, label string) {
	t.Helper()

	packetReceived := make(chan struct{})
	go func() {
		for range count {
			if _, _, err := track.ReadRTP(); err != nil {
				return
			}
		}
		close(packetReceived)
	}()

	select {
	case <-packetReceived:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %d RTP packets for %s", count, label)
	}
}

func waitForCondition(t *testing.T, label string, timeout time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}

		time.Sleep(40 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", label)
}

func startBroadcastLoop(t *testing.T, track *webrtc.TrackLocalStaticSample, frame []byte) chan struct{} {
	t.Helper()

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := track.WriteSample(media.Sample{Data: frame, Duration: 40 * time.Millisecond}); err != nil {
					return
				}
			}
		}
	}()

	return stop
}

func firstVideoFrame(t *testing.T, data []byte) []byte {
	t.Helper()

	reader, _, err := ivfreader.NewWith(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to create IVF reader: %v", err)
	}

	frame, _, err := reader.ParseNextFrame()
	if err != nil {
		t.Fatalf("failed to parse first IVF frame: %v", err)
	}

	return frame
}

func h264TestSample() []byte {
	return []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0xC0, 0x1F, 0xDA, 0x02, 0x80, 0xB7, 0xFE, 0x05, 0x01, 0x01, 0x01, 0x40,
		0x00, 0x00, 0x00, 0x01, 0x68, 0xCE, 0x06, 0xE2,
		0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x21, 0xA0,
	}
}
