const remoteVideo = document.getElementById("remoteVideo");
const resumeOverlay = document.getElementById("resumeOverlay");
const resumeOverlayLabel = document.getElementById("resumeOverlayLabel");
const resumeOverlayDetail = document.getElementById("resumeOverlayDetail");
const statusLine = document.getElementById("status");
const eyeButton = document.getElementById("cameraEyeButton");
const frameIndicator = document.getElementById("frameIndicator");
const scene = document.getElementById("scene");
const stage = document.getElementById("stage");
const debugPanel = document.getElementById("debugPanel");
const debugToggle = document.getElementById("debugToggle");
const channelButtons = [...document.querySelectorAll("[data-channel-slot]")];

const debugRefs = {
  signalingState: document.getElementById("signalingState"),
  connectionState: document.getElementById("connectionState"),
  iceConnectionState: document.getElementById("iceConnectionState"),
  iceGatheringState: document.getElementById("iceGatheringState"),
  modeState: document.getElementById("modeState"),
  sourceState: document.getElementById("sourceState"),
  remoteTrackInfo: document.getElementById("remoteTrackInfo"),
  videoDimensions: document.getElementById("videoDimensions"),
  codecInfo: document.getElementById("codecInfo"),
  bytesReceived: document.getElementById("bytesReceived"),
  bitrate: document.getElementById("bitrate"),
  framesDecoded: document.getElementById("framesDecoded"),
  packetsLost: document.getElementById("packetsLost"),
  timingInfo: document.getElementById("timingInfo"),
  candidatePairInfo: document.getElementById("candidatePairInfo"),
  offerSdp: document.getElementById("offerSdp"),
  answerSdp: document.getElementById("answerSdp"),
  offerReadable: document.getElementById("offerReadable"),
  answerReadable: document.getElementById("answerReadable"),
  eventLog: document.getElementById("eventLog"),
};

const panelButtons = [...document.querySelectorAll("[data-panel-target]")];
const panelViews = [...document.querySelectorAll("[data-panel-view]")];
const sdpModeButtons = [...document.querySelectorAll("[data-sdp-target]")];
const sdpModePanes = [...document.querySelectorAll("[data-sdp-pane]")];
const copyButtons = [...document.querySelectorAll("[data-copy-source]")];
const eventEntries = [];

const supersededConnectionMessage = "Connection attempt superseded";
const viewerRole = "viewer";
const cameraRole = "camera";
const controlChannelLabel = "tv-control";
const offerKindInitial = "initial";
const offerKindIceRestart = "ice-restart";
const signalingCodeRestartRequiresReconnect = "restart_requires_full_reconnect";
const signalingCodeCameraSlotsFull = "camera_slots_full";
const interruptedOverlayMessage = "Connection interrupted";
const recentFrameWindowMs = 1200;
const iceReconnectDelaysMs = [3000, 3000, 10000];
const fullReconnectInitialDelaysMs = [0, 3000, 10000];
const fullReconnectRepeatDelayMs = 30000;
const reconnectAttemptTimeoutMs = 5000;
const maxCameraSlots = 3;
const cameraSlotLimitMessage = "Camera limit reached";
const startingCameraMessage = "Starting Camera";
const startingVideoMessage = "Starting Video";
const temporaryOverlayDurationMs = 5000;

let currentSession = null;
let debugUiInstalled = false;
let lastStatsSnapshot = null;
let resumeCheckTimer = null;
let activeSessionToken = 0;
let overlayRecoveryMode = "resume";
let resumeOverlayMessage = "";
let resumeOverlayDetailMessage = "";
let requestedRole = viewerRole;
let localCameraStream = null;
let debugPanelOpen = false;
let streamStallState = {
  lastFramesDecoded: null,
  lastDisplayedFrames: null,
  stalledIntervals: 0,
  hasSeenProgress: false,
};
let frameMonitorInstalled = false;
let displayedFrameCount = 0;
let lastPlaybackFrameMetric = null;
let lastDisplayedFrameAtMs = 0;
let cameraAckState = {
  framesReceived: 0,
  lastFrameSeenAtMs: 0,
  lastFrameAtMs: 0,
  channelOpen: false,
};
let selectedChannelSlot = 1;
let assignedCameraSlot = 0;
let channelSlotStates = Array.from({ length: maxCameraSlots }, (_, index) => ({
  slot: index + 1,
  occupied: false,
  live: false,
}));
let debugPanelSpaceFrame = null;
let reconnectTimer = null;
let reconnectSequenceId = 0;
let reconnectTrackedToken = null;
let temporaryOverlayTimer = null;
let cameraStartupPending = false;
let cameraStartupTimer = null;
let viewerStartupPending = false;

function setMetric(name, value) {
  debugRefs[name].textContent = value;
}

function appendLog(message) {
  const timestamp = new Date().toLocaleTimeString();
  eventEntries.push(`[${timestamp}] ${message}`);
  debugRefs.eventLog.textContent = eventEntries.slice(-80).join("\n");
}

function setStatus(message) {
  statusLine.textContent = message;
  appendLog(message);
}

function markFrameSeen() {
  lastDisplayedFrameAtMs = performance.now();
}

function recordDisplayedFrames(count = 1) {
  if (!Number.isFinite(count) || count <= 0) {
    return;
  }

  displayedFrameCount += count;
  markFrameSeen();

  if (viewerStartupPending && requestedRole === viewerRole) {
    finishViewerStartupOverlay();
  }
}

function getPlaybackFrameCount() {
  if (typeof remoteVideo.getVideoPlaybackQuality === "function") {
    const quality = remoteVideo.getVideoPlaybackQuality();
    if (Number.isFinite(quality.totalVideoFrames)) {
      return quality.totalVideoFrames;
    }
  }

  if (Number.isFinite(remoteVideo.webkitDecodedFrameCount)) {
    return remoteVideo.webkitDecodedFrameCount;
  }

  return null;
}

function setFrameIndicatorState(state) {
  frameIndicator.classList.remove("is-amber", "is-green", "is-red");
  if (state === "green") {
    frameIndicator.classList.add("is-green");
    return;
  }

  if (state === "red") {
    frameIndicator.classList.add("is-red");
    return;
  }

  frameIndicator.classList.add("is-amber");
}

function sessionHasHealthyConnection(session = currentSession) {
  if (!session?.pc) {
    return false;
  }

  return (
    session.pc.connectionState === "connected" &&
    ["connected", "completed"].includes(session.pc.iceConnectionState)
  );
}

function hasRecentFrame(now = performance.now()) {
  if (currentSession?.role === cameraRole) {
    return cameraAckState.lastFrameSeenAtMs > 0 && (now - cameraAckState.lastFrameSeenAtMs) <= recentFrameWindowMs;
  }

  return lastDisplayedFrameAtMs > 0 && (now - lastDisplayedFrameAtMs) <= recentFrameWindowMs;
}

function updateFrameIndicatorState() {
  if (!sessionHasHealthyConnection()) {
    setFrameIndicatorState("amber");
    return;
  }

  setFrameIndicatorState(hasRecentFrame() ? "green" : "red");
}

function pollPlaybackFrames() {
  const frameCount = getPlaybackFrameCount();
  if (frameCount === null) {
    return;
  }

  if (lastPlaybackFrameMetric !== null && frameCount > lastPlaybackFrameMetric) {
    recordDisplayedFrames(frameCount - lastPlaybackFrameMetric);
  }

  lastPlaybackFrameMetric = frameCount;
}

function resetPlaybackFrameTracking() {
  displayedFrameCount = 0;
  lastPlaybackFrameMetric = null;
  lastDisplayedFrameAtMs = 0;
  updateFrameIndicatorState();
}

function resetCameraAckState() {
  cameraAckState = {
    framesReceived: 0,
    lastFrameSeenAtMs: 0,
    lastFrameAtMs: 0,
    channelOpen: false,
  };
  updateFrameIndicatorState();
}

function clearCameraStartupTimer() {
  if (cameraStartupTimer === null) {
    return;
  }

  window.clearTimeout(cameraStartupTimer);
  cameraStartupTimer = null;
}

function cameraStartupOverlayActive() {
  return cameraStartupPending && cameraAckState.framesReceived === 0;
}

function awaitingFirstCameraFrame() {
  return cameraAckState.framesReceived === 0 && (cameraStartupPending || requestedRole === cameraRole);
}

function viewerStartupOverlayActive() {
  return viewerStartupPending && !hasDisplayedFrameProgress();
}

function awaitingFirstViewerFrame() {
  return viewerStartupPending && !hasDisplayedFrameProgress();
}

function awaitingFirstStartupFrame() {
  return awaitingFirstCameraFrame() || awaitingFirstViewerFrame();
}

function showStartupOverlayIfAwaiting() {
  if (cameraStartupOverlayActive()) {
    showReconnectOverlay(startingCameraMessage, "Starting camera");
    return true;
  }

  if (viewerStartupOverlayActive()) {
    showReconnectOverlay(startingVideoMessage, "Starting video");
    return true;
  }

  return false;
}

function hideResumeOverlayIfReady() {
  if (awaitingFirstStartupFrame()) {
    return;
  }

  hideResumeOverlay();
}

function startCameraStartupOverlay() {
  clearCameraStartupTimer();
  cameraStartupPending = true;
  showReconnectOverlay(startingCameraMessage, "Starting camera");
  setStatus("Starting camera...");
}

function startViewerStartupOverlay() {
  viewerStartupPending = true;
  showReconnectOverlay(startingVideoMessage, "Starting video");
  setStatus("Starting video...");
}

function finishCameraStartupOverlay({ keepOverlay = false } = {}) {
  clearCameraStartupTimer();
  if (!cameraStartupPending) {
    return;
  }

  cameraStartupPending = false;
  if (!keepOverlay && requestedRole === cameraRole && currentSession?.role === cameraRole) {
    hideResumeOverlayIfReady();
  }
}

function finishViewerStartupOverlay({ keepOverlay = false } = {}) {
  if (!viewerStartupPending) {
    return;
  }

  viewerStartupPending = false;
  if (!keepOverlay && requestedRole === viewerRole && currentSession?.role === viewerRole) {
    hideResumeOverlayIfReady();
  }
}

function handoffReconnectOverlayToStartup(session = currentSession) {
  if (!session) {
    return;
  }

  if (session.role === cameraRole) {
    if (cameraAckState.framesReceived > 0) {
      return;
    }

    startCameraStartupOverlay();
    armCameraStartupTimer(session.token);
    return;
  }

  if (!hasDisplayedFrameProgress()) {
    startViewerStartupOverlay();
  }
}

function armCameraStartupTimer(sessionToken) {
  clearCameraStartupTimer();
  if (!isCurrentSession(sessionToken) || currentSession?.role !== cameraRole || !awaitingFirstCameraFrame()) {
    return;
  }

  cameraStartupTimer = window.setTimeout(() => {
    cameraStartupTimer = null;
    if (!isCurrentSession(sessionToken) || currentSession?.role !== cameraRole || !awaitingFirstCameraFrame()) {
      return;
    }

    appendLog("Camera startup timed out before the first frame. Starting full reconnect.");
    finishCameraStartupOverlay({ keepOverlay: true });
    startImmediateFullReconnect("Camera startup timed out");
  }, reconnectAttemptTimeoutMs);
}

function normalizeChannelSlot(slot) {
  const parsed = Number(slot);
  if (!Number.isFinite(parsed) || parsed < 1 || parsed > maxCameraSlots) {
    return 1;
  }

  return Math.trunc(parsed);
}

function applyChannelState(payload) {
  if (Array.isArray(payload.slots)) {
    channelSlotStates = Array.from({ length: maxCameraSlots }, (_, index) => {
      const slot = index + 1;
      const next = payload.slots.find((candidate) => normalizeChannelSlot(candidate.slot) === slot);
      return {
        slot,
        occupied: Boolean(next?.occupied),
        live: Boolean(next?.live),
      };
    });
  }

  if (Number.isFinite(Number(payload.selectedSlot)) && requestedRole !== cameraRole) {
    selectedChannelSlot = normalizeChannelSlot(payload.selectedSlot);
  }

  if (Number.isFinite(Number(payload.assignedSlot))) {
    assignedCameraSlot = normalizeChannelSlot(payload.assignedSlot);
  } else if (requestedRole !== cameraRole) {
    assignedCameraSlot = 0;
  }

  updateChannelButtons();
}

function handleCameraAckMessage(token, payload) {
  if (!isCurrentSession(token) || currentSession?.role !== cameraRole) {
    return;
  }

  if (payload.type !== "cameraAck") {
    return;
  }

  const framesReceived = Number(payload.framesReceived);
  if (Number.isFinite(framesReceived) && framesReceived > cameraAckState.framesReceived) {
    cameraAckState.framesReceived = framesReceived;
    cameraAckState.lastFrameSeenAtMs = performance.now();
    finishCameraStartupOverlay();
  }

  const lastFrameAtMs = Number(payload.lastFrameAtMs);
  if (Number.isFinite(lastFrameAtMs) && lastFrameAtMs > 0) {
    cameraAckState.lastFrameAtMs = lastFrameAtMs;
  }

  updateFrameIndicatorState();
}

function sendSelectedChannelChange(slot = selectedChannelSlot) {
  if (currentSession?.role !== viewerRole || currentSession?.controlChannel?.readyState !== "open") {
    return;
  }

  currentSession.controlChannel.send(JSON.stringify({
    type: "selectChannel",
    slot: normalizeChannelSlot(slot),
  }));
}

function handleControlMessage(token, payload) {
  if (!isCurrentSession(token)) {
    return;
  }

  if (payload.type === "cameraAck") {
    handleCameraAckMessage(token, payload);
    return;
  }

  if (payload.type === "channelState") {
    applyChannelState(payload);
  }
}

function installControlChannel(pc, token, role) {
  const channel = pc.createDataChannel(controlChannelLabel, { ordered: true });
  currentSession.controlChannel = channel;

  channel.addEventListener("open", () => {
    if (!isCurrentSession(token)) {
      return;
    }

    if (role === cameraRole) {
      cameraAckState.channelOpen = true;
      appendLog("Camera control channel open");
    } else {
      appendLog("Viewer control channel open");
      sendSelectedChannelChange();
    }

    updateFrameIndicatorState();
  });

  channel.addEventListener("close", () => {
    if (!isCurrentSession(token)) {
      return;
    }

    if (role === cameraRole) {
      cameraAckState.channelOpen = false;
      appendLog("Camera control channel closed");
    } else {
      appendLog("Viewer control channel closed");
    }

    updateFrameIndicatorState();
  });

  channel.addEventListener("message", (event) => {
    if (!isCurrentSession(token)) {
      return;
    }

    try {
      handleControlMessage(token, JSON.parse(event.data));
    } catch (error) {
      console.error(error);
      appendLog(`Control payload parse failed: ${error.message}`);
    }
  });
}

function hasDisplayedFrameProgress() {
  return displayedFrameCount > 0;
}

function installFrameIndicatorMonitor() {
  if (frameMonitorInstalled) {
    return;
  }

  frameMonitorInstalled = true;

  if (typeof remoteVideo.requestVideoFrameCallback === "function") {
    const watchRenderedFrame = () => {
      remoteVideo.requestVideoFrameCallback(() => {
        recordDisplayedFrames();
        watchRenderedFrame();
      });
    };

    watchRenderedFrame();
  } else {
    window.setInterval(pollPlaybackFrames, 250);
  }

  window.setInterval(() => {
    updateFrameIndicatorState();
  }, 250);

  updateFrameIndicatorState();
}

function setScreenStream(stream, { mirror = false } = {}) {
  if (remoteVideo.srcObject !== stream) {
    remoteVideo.srcObject = stream;
    resetPlaybackFrameTracking();
  }

  remoteVideo.classList.toggle("local-preview", mirror);

  if (stream) {
    remoteVideo.play().catch(() => {});
  }
}

function setRoleMetric(role = requestedRole) {
  setMetric("modeState", role === cameraRole ? "Camera" : "Viewer");
}

function setSourceMetric(source) {
  setMetric("sourceState", source);
}

function updateChannelButtons() {
  const cameraActive = requestedRole === cameraRole;
  const highlightedSlot = cameraActive && assignedCameraSlot > 0 ? assignedCameraSlot : selectedChannelSlot;

  channelButtons.forEach((button) => {
    const slot = normalizeChannelSlot(button.dataset.channelSlot);
    const label = button.dataset.channelLabel || "";
    const slotState = channelSlotStates.find((candidate) => candidate.slot === slot) || { occupied: false, live: false };
    const enabled = slotState.occupied && !cameraActive;
    const active = slot === highlightedSlot && slotState.occupied;

    button.disabled = !enabled;
    button.textContent = slotState.occupied ? label : "";

    button.classList.toggle("is-active", active);
    button.classList.toggle("is-available", slotState.occupied && !active);
    button.classList.toggle("is-empty", !slotState.occupied);

    if (slotState.occupied) {
      const labelText = active
        ? `Channel ${slot}${slotState.live ? " live and selected" : " selected"}`
        : `Channel ${slot}${slotState.live ? " live" : " available"}`;
      button.setAttribute("aria-label", cameraActive ? `${labelText}; unavailable while camera is active` : labelText);
      return;
    }

    button.setAttribute(
      "aria-label",
      cameraActive ? `Channel ${slot} unavailable while camera is active` : `Channel ${slot} unavailable`,
    );
  });
}

function updateEyeButton() {
  const active = requestedRole === cameraRole;
  eyeButton.classList.toggle("is-active", active);
  eyeButton.setAttribute("aria-pressed", String(active));
  eyeButton.setAttribute(
    "aria-label",
    active ? "Return to viewer mode" : "Activate camera broadcast mode",
  );
  setRoleMetric();
  setSourceMetric(active ? "Local camera" : "Server feed");
  updateChannelButtons();
}

function describeCameraError(error) {
  if (error.name === "NotAllowedError") {
    if (window.isSecureContext === false) {
      return "Camera access needs HTTPS on iPhone Safari.";
    }

    return "Camera access was denied.";
  }

  if (error.name === "NotFoundError") {
    return "No camera was found on this device.";
  }

  if (error.name === "NotReadableError" || error.name === "AbortError") {
    return "The camera is busy in another app or tab.";
  }

  return error.message || "Camera mode failed.";
}

async function ensureLocalCameraStream() {
  if (!navigator.mediaDevices?.getUserMedia) {
    throw new Error("This browser does not expose camera capture here.");
  }

  const existingTrack = localCameraStream?.getVideoTracks().find((track) => track.readyState === "live");
  if (existingTrack) {
    return localCameraStream;
  }

  localCameraStream = await navigator.mediaDevices.getUserMedia({
    audio: false,
    video: {
      facingMode: "user",
      width: { ideal: 1280 },
      height: { ideal: 720 },
    },
  });

  return localCameraStream;
}

function forcedCameraVP8Codecs() {
  if (typeof RTCRtpSender === "undefined" || typeof RTCRtpSender.getCapabilities !== "function") {
    throw new Error("This browser cannot force VP8 camera upload.");
  }

  const capabilities = RTCRtpSender.getCapabilities("video");
  if (!capabilities?.codecs?.length) {
    throw new Error("Video codec capabilities are unavailable here.");
  }

  const allowedMimeTypes = new Set(["video/vp8", "video/rtx", "video/red", "video/ulpfec"]);
  const codecs = capabilities.codecs.filter((codec) => allowedMimeTypes.has(codec.mimeType.toLowerCase()));
  const vp8Codecs = codecs.filter((codec) => codec.mimeType.toLowerCase() === "video/vp8");

  if (!vp8Codecs.length) {
    throw new Error("This browser does not support VP8 camera upload.");
  }

  return [...vp8Codecs, ...codecs.filter((codec) => codec.mimeType.toLowerCase() !== "video/vp8")];
}

function releaseLocalCamera() {
  if (!localCameraStream) {
    return;
  }

  localCameraStream.getTracks().forEach((track) => {
    track.stop();
  });
  localCameraStream = null;
}

async function fetchCameraSlots() {
  const response = await fetch("/camera-slots");
  if (!response.ok) {
    throw new Error("Failed to read camera slot status.");
  }

  const payload = await response.json();
  applyChannelState(payload);
  return payload;
}

function hasAvailableCameraSlot(slots = channelSlotStates) {
  return slots.some((slot) => !slot.occupied);
}

function showCameraSlotLimitReached() {
  setStatus("Camera limit reached.");
  showTemporaryReconnectOverlay(cameraSlotLimitMessage);
}

function overlayAriaLabel(label, detail) {
  if (label && detail) {
    return `${label}. ${detail}`;
  }

  return label || "Resume video playback";
}

function showResumeOverlay(reason, mode = "resume", label = "", detail = "") {
  overlayRecoveryMode = mode;
  resumeOverlayMessage = label;
  resumeOverlayDetailMessage = detail;
  resumeOverlay.dataset.mode = mode;
  resumeOverlayLabel.textContent = label;
  resumeOverlayDetail.textContent = detail;
  resumeOverlay.setAttribute("aria-label", overlayAriaLabel(label, detail));

  if (document.visibilityState === "hidden") {
    return;
  }

  if (resumeOverlay.hidden) {
    appendLog(`${reason}. Showing ${mode === "reconnect" ? "recovery" : "play"} overlay.`);
  }

  resumeOverlay.hidden = false;
}

function hideResumeOverlay() {
  window.clearTimeout(temporaryOverlayTimer);
  temporaryOverlayTimer = null;
  overlayRecoveryMode = "resume";
  resumeOverlayMessage = "";
  resumeOverlayDetailMessage = "";
  resumeOverlay.dataset.mode = "resume";
  resumeOverlayLabel.textContent = "";
  resumeOverlayDetail.textContent = "";
  resumeOverlay.setAttribute("aria-label", "Resume video playback");
  resumeOverlay.hidden = true;
}

function showReconnectOverlay(message, reason = message, detail = "") {
  showResumeOverlay(reason, "reconnect", message, detail);
}

function showTemporaryReconnectOverlay(message, duration = temporaryOverlayDurationMs) {
  showReconnectOverlay(message);
  window.clearTimeout(temporaryOverlayTimer);
  temporaryOverlayTimer = window.setTimeout(() => {
    temporaryOverlayTimer = null;
    if (overlayRecoveryMode === "reconnect" && resumeOverlayMessage === message && !sessionRequiresReconnect()) {
      hideResumeOverlay();
    }
  }, duration);
}

function setReconnectOverlayDetail(detail = "") {
  resumeOverlayDetailMessage = detail;
  resumeOverlayDetail.textContent = detail;
  resumeOverlay.setAttribute("aria-label", overlayAriaLabel(resumeOverlayMessage, detail));
}

function formatBackoffDetail(delay) {
  return "Failed - backoff before retry";
}

function resetStreamStallState() {
  streamStallState = {
    lastFramesDecoded: null,
    lastDisplayedFrames: null,
    stalledIntervals: 0,
    hasSeenProgress: false,
  };

  resetPlaybackFrameTracking();
}

async function attemptResumePlayback(reason, showOverlayOnFailure = true) {
  if (!remoteVideo.srcObject || document.visibilityState === "hidden") {
    return false;
  }

  if (showStartupOverlayIfAwaiting()) {
    return false;
  }

  try {
    await remoteVideo.play();
    hideResumeOverlayIfReady();
    appendLog(`${reason}. Playback resumed.`);
    return true;
  } catch (error) {
    console.error(error);
    appendLog(`${reason}. Resume attempt failed: ${error.message}`);
    if (showOverlayOnFailure) {
      if (showStartupOverlayIfAwaiting()) {
        return false;
      }
      showResumeOverlay(reason, "resume");
    }
    return false;
  }
}

function scheduleResumeCheck(reason, delay = 250) {
  window.clearTimeout(resumeCheckTimer);
  resumeCheckTimer = window.setTimeout(() => {
    if (showStartupOverlayIfAwaiting()) {
      return;
    }

    if (!remoteVideo.srcObject || document.visibilityState === "hidden") {
      return;
    }

    if (!remoteVideo.paused) {
      hideResumeOverlayIfReady();
      return;
    }

    attemptResumePlayback(reason).then((resumed) => {
      if (resumed) {
        return;
      }

      if (remoteVideo.paused) {
        showResumeOverlay(reason, "resume");
      }
    });
  }, delay);
}

function handleStalledPlayback(reason) {
  if (document.visibilityState === "hidden") {
    return;
  }

  if (showStartupOverlayIfAwaiting()) {
    return;
  }

  if (remoteVideo.srcObject && remoteVideo.paused) {
    scheduleResumeCheck(reason, 120);
    return;
  }

  showReconnectOverlay(resumeOverlayMessage || reason, reason);
}

function activatePanel(name) {
  panelButtons.forEach((button) => {
    const active = button.dataset.panelTarget === name;
    button.classList.toggle("is-active", active);
    button.setAttribute("aria-pressed", String(active));
  });

  panelViews.forEach((view) => {
    view.hidden = view.dataset.panelView !== name;
  });

  syncDebugPanelSpace();
}

function syncDebugPanelSpace() {
  if (debugPanelSpaceFrame !== null) {
    window.cancelAnimationFrame(debugPanelSpaceFrame);
  }

  debugPanelSpaceFrame = window.requestAnimationFrame(() => {
    debugPanelSpaceFrame = null;

    if (!debugPanelOpen || getComputedStyle(debugPanel).position !== "absolute") {
      document.documentElement.style.setProperty("--debug-panel-space", "0px");
      return;
    }

    const stageRect = stage.getBoundingClientRect();
    const panelRect = debugPanel.getBoundingClientRect();
    const extraSpace = Math.max(0, panelRect.bottom - stageRect.bottom);
    document.documentElement.style.setProperty("--debug-panel-space", `${Math.ceil(extraSpace)}px`);
  });
}

function setDebugPanelOpen(open) {
  debugPanelOpen = open;
  scene.classList.toggle("has-debug", open);
  debugPanel.hidden = !open;
  debugToggle.classList.toggle("is-active", open);
  debugToggle.setAttribute("aria-expanded", String(open));
  debugToggle.setAttribute(
    "aria-label",
    open ? "Hide WebRTC debug panel" : "Show WebRTC debug panel",
  );
  syncDebugPanelSpace();
}

function activateSdpMode(target, mode) {
  sdpModeButtons.forEach((button) => {
    const active = button.dataset.sdpTarget === target && button.dataset.sdpMode === mode;
    if (button.dataset.sdpTarget === target) {
      button.classList.toggle("is-active", active);
      button.setAttribute("aria-pressed", String(active));
    }
  });

  sdpModePanes.forEach((pane) => {
    if (pane.dataset.sdpPane !== target) {
      return;
    }

    pane.hidden = pane.dataset.sdpPaneMode !== mode;
  });

  syncDebugPanelSpace();
}

function waitForIceGathering(pc) {
  if (pc.iceGatheringState === "complete") {
    return Promise.resolve();
  }

  return new Promise((resolve) => {
    const onStateChange = () => {
      if (pc.iceGatheringState !== "complete") {
        return;
      }

      pc.removeEventListener("icegatheringstatechange", onStateChange);
      resolve();
    };

    pc.addEventListener("icegatheringstatechange", onStateChange);
  });
}

function createSessionId() {
  if (globalThis.crypto?.randomUUID) {
    return globalThis.crypto.randomUUID();
  }

  return `session-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function clearReconnectTimer() {
  if (reconnectTimer === null) {
    return;
  }

  window.clearTimeout(reconnectTimer);
  reconnectTimer = null;
}

function cancelReconnectSequence() {
  reconnectSequenceId += 1;
  reconnectTrackedToken = null;
  clearReconnectTimer();
}

function sessionRecovered(session = currentSession) {
  if (!session?.pc) {
    return false;
  }

  if (session.role === cameraRole && cameraAckState.framesReceived === 0) {
    return false;
  }

  if (session.role === viewerRole && awaitingFirstViewerFrame()) {
    return false;
  }

  return (
    ["connected", "completed"].includes(session.pc.iceConnectionState) ||
    session.pc.connectionState === "connected"
  );
}

function scheduleReconnectStep(sequenceId, delay, action, { showBackoffDetail = true } = {}) {
  clearReconnectTimer();
  if (showBackoffDetail && delay > 0) {
    setReconnectOverlayDetail(formatBackoffDetail(delay));
  } else {
    setReconnectOverlayDetail("");
  }

  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = null;
    if (sequenceId !== reconnectSequenceId) {
      return;
    }

    setReconnectOverlayDetail("");
    action().catch((error) => {
      if (error.message !== supersededConnectionMessage) {
        reportError(error);
      }
    });
  }, delay);
}

function waitForConnectionRecovery(sessionToken, timeout = reconnectAttemptTimeoutMs) {
  if (!isCurrentSession(sessionToken)) {
    return Promise.resolve(false);
  }

  const session = currentSession;
  if (!session?.pc) {
    return Promise.resolve(false);
  }

  if (sessionRecovered(session)) {
    return Promise.resolve(true);
  }

  const { pc } = session;
  return new Promise((resolve) => {
    let settled = false;

    const finish = (result) => {
      if (settled) {
        return;
      }

      settled = true;
      window.clearTimeout(timer);
      pc.removeEventListener("iceconnectionstatechange", onStateChange);
      pc.removeEventListener("connectionstatechange", onStateChange);
      resolve(result);
    };

    const onStateChange = () => {
      if (!isCurrentSession(sessionToken)) {
        finish(false);
        return;
      }

      if (sessionRecovered(currentSession)) {
        finish(true);
        return;
      }

      if (
        ["failed", "closed"].includes(pc.iceConnectionState) ||
        ["failed", "closed"].includes(pc.connectionState)
      ) {
        finish(false);
      }
    };

    const timer = window.setTimeout(() => {
      finish(isCurrentSession(sessionToken) && sessionRecovered(currentSession));
    }, timeout);

    pc.addEventListener("iceconnectionstatechange", onStateChange);
    pc.addEventListener("connectionstatechange", onStateChange);
  });
}

function formatBytes(value) {
  if (!Number.isFinite(value) || value <= 0) {
    return "0 B";
  }

  const units = ["B", "KB", "MB", "GB"];
  let size = value;
  let unitIndex = 0;

  while (size >= 1024 && unitIndex < units.length - 1) {
    size /= 1024;
    unitIndex += 1;
  }

  return `${size.toFixed(size >= 10 || unitIndex === 0 ? 0 : 1)} ${units[unitIndex]}`;
}

function formatBitrate(bytesDelta, millisecondsDelta) {
  if (!Number.isFinite(bytesDelta) || !Number.isFinite(millisecondsDelta) || millisecondsDelta <= 0) {
    return "pending";
  }

  const bitsPerSecond = (bytesDelta * 8 * 1000) / millisecondsDelta;
  if (!Number.isFinite(bitsPerSecond) || bitsPerSecond <= 0) {
    return "0 kbps";
  }

  if (bitsPerSecond >= 1_000_000) {
    return `${(bitsPerSecond / 1_000_000).toFixed(2)} Mbps`;
  }

  return `${(bitsPerSecond / 1_000).toFixed(0)} kbps`;
}

function formatTiming(jitter, roundTripTime) {
  const jitterMs = Number.isFinite(jitter) ? `${(jitter * 1000).toFixed(1)} ms` : "n/a";
  const rttMs = Number.isFinite(roundTripTime) ? `${(roundTripTime * 1000).toFixed(1)} ms` : "n/a";
  return `${jitterMs} / ${rttMs}`;
}

function formatCandidate(candidate) {
  if (!candidate) {
    return "pending";
  }

  const address = candidate.address || candidate.ip || "unknown";
  const port = candidate.port ?? "?";
  return `${candidate.candidateType || "unknown"} ${candidate.protocol || ""} ${address}:${port}`.trim();
}

function escapeHtml(value) {
  return String(value).replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
}

function updateStaticState(pc) {
  setMetric("signalingState", pc.signalingState);
  setMetric("connectionState", pc.connectionState);
  setMetric("iceConnectionState", pc.iceConnectionState);
  setMetric("iceGatheringState", pc.iceGatheringState);
  updateFrameIndicatorState();
}

function resetLiveMetrics() {
  setRoleMetric();
  setSourceMetric(requestedRole === cameraRole ? "Local camera" : "Server feed");
  setMetric("remoteTrackInfo", "pending");
  setMetric("videoDimensions", "pending");
  setMetric("codecInfo", "pending");
  setMetric("bytesReceived", "pending");
  setMetric("bitrate", "pending");
  setMetric("framesDecoded", "pending");
  setMetric("packetsLost", "pending");
  setMetric("timingInfo", "pending");
  setMetric("candidatePairInfo", "pending");
}

function isCurrentSession(token) {
  return currentSession?.token === token;
}

function assertCurrentSession(token) {
  if (!isCurrentSession(token)) {
    throw new Error(supersededConnectionMessage);
  }
}

function clearSessionStats(session) {
  if (!session?.statsTimer) {
    return;
  }

  window.clearInterval(session.statsTimer);
  session.statsTimer = null;
}

function disposeSession(session) {
  if (!session) {
    return;
  }

  clearSessionStats(session);

  if (session.pc && session.pc.signalingState !== "closed") {
    session.pc.close();
  }
}

function ensureSessionStatsTimer(session) {
  if (!session?.pc || session.statsTimer) {
    return;
  }

  const { token, pc } = session;
  session.statsTimer = window.setInterval(() => {
    if (!isCurrentSession(token)) {
      return;
    }

    refreshStats(pc).catch((error) => {
      console.error(error);
    });
  }, 1000);
}

async function negotiateSession(session, { iceRestart = false } = {}) {
  const { pc, role, sessionId, token } = session;
  const offerKind = iceRestart ? offerKindIceRestart : offerKindInitial;

  setStatus(iceRestart ? "Creating ICE restart offer..." : "Creating local offer...");
  const offer = await pc.createOffer(iceRestart ? { iceRestart: true } : undefined);
  assertCurrentSession(token);

  await pc.setLocalDescription(offer);
  await waitForIceGathering(pc);
  assertCurrentSession(token);

  updateSdpViews("offer", pc.localDescription?.sdp || "");
  appendLog(iceRestart ? "Browser ICE restart offer created." : "Browser SDP offer created.");

  setStatus(iceRestart ? "Sending ICE restart offer to Go/Pion server..." : "Sending offer to Go/Pion server...");
  const response = await fetch("/offer", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      sessionId,
      offerKind,
      selectedSlot: selectedChannelSlot,
      role,
      type: pc.localDescription?.type,
      sdp: pc.localDescription?.sdp,
    }),
  });
  assertCurrentSession(token);

  appendLog(`POST /offer -> ${response.status} ${response.statusText}`);

  if (!response.ok) {
    const responseError = await response.json().catch(() => ({ error: response.statusText }));
    const error = new Error(responseError.error || "signaling request failed");
    if (typeof responseError.code === "string" && responseError.code) {
      error.code = responseError.code;
    }
    throw error;
  }

  const answer = await response.json();
  assertCurrentSession(token);

  updateSdpViews("answer", answer.sdp || "");
  appendLog(iceRestart ? "Server ICE restart answer received." : "Server SDP answer received.");

  await pc.setRemoteDescription(answer);
  assertCurrentSession(token);

  updateStaticState(pc);
}

function requiresFullReconnect(error) {
  return error?.code === signalingCodeRestartRequiresReconnect;
}

function cameraSlotsFull(error) {
  return error?.code === signalingCodeCameraSlotsFull;
}

function scheduleFullReconnectAttempt(sequenceId, attemptIndex = 0) {
  const delay = attemptIndex < fullReconnectInitialDelaysMs.length
    ? fullReconnectInitialDelaysMs[attemptIndex]
    : fullReconnectRepeatDelayMs;

  scheduleReconnectStep(sequenceId, delay, async () => {
    const label = `Full reconnect ${attemptIndex + 1}`;
    showReconnectOverlay(label);
    setStatus(label);

    try {
      const session = await connectToServer(label, requestedRole, {
        preserveRecoveryOverlay: true,
        sessionId: createSessionId(),
      });
      reconnectTrackedToken = session.token;
      handoffReconnectOverlayToStartup(session);

      if (await waitForConnectionRecovery(session.token)) {
        cancelReconnectSequence();
        hideResumeOverlay();
        ensureSessionStatsTimer(session);
        return;
      }
    } catch (error) {
      if (error.message !== supersededConnectionMessage) {
        appendLog(`${label} failed: ${error.message}`);
      }
    }

    if (sequenceId === reconnectSequenceId) {
      scheduleFullReconnectAttempt(sequenceId, attemptIndex + 1);
    }
  });
}

async function runIceReconnectAttempt(sequenceId, attemptIndex) {
  if (sequenceId !== reconnectSequenceId || !currentSession?.pc) {
    return;
  }

  const sessionToken = currentSession.token;
  const label = `ICE reconnect ${attemptIndex + 1}/3`;
  showReconnectOverlay(label);
  setStatus(label);

  try {
    await negotiateSession(currentSession, { iceRestart: true });
    if (await waitForConnectionRecovery(sessionToken)) {
      cancelReconnectSequence();
      hideResumeOverlay();
      ensureSessionStatsTimer(currentSession);
      return;
    }
  } catch (error) {
    if (requiresFullReconnect(error)) {
      appendLog(`${label} escalated to full reconnect.`);
      if (sequenceId === reconnectSequenceId) {
        scheduleFullReconnectAttempt(sequenceId, 0);
      }
      return;
    }

    if (error.message !== supersededConnectionMessage) {
      appendLog(`${label} failed: ${error.message}`);
    }
  }

  if (sequenceId !== reconnectSequenceId) {
    return;
  }

  if (attemptIndex + 1 < iceReconnectDelaysMs.length) {
    scheduleReconnectStep(sequenceId, iceReconnectDelaysMs[attemptIndex + 1], () =>
      runIceReconnectAttempt(sequenceId, attemptIndex + 1));
    return;
  }

  scheduleFullReconnectAttempt(sequenceId, 0);
}

function startReconnectSequence(initialDelay = iceReconnectDelaysMs[0]) {
  if (!currentSession?.pc) {
    return;
  }

  if (currentSession.role === cameraRole && cameraAckState.framesReceived === 0) {
    startImmediateFullReconnect("Camera startup interrupted");
    return;
  }

  finishViewerStartupOverlay({ keepOverlay: true });

  if (reconnectTrackedToken === currentSession.token) {
    return;
  }

  cancelReconnectSequence();
  reconnectTrackedToken = currentSession.token;
  const sequenceId = reconnectSequenceId;
  showReconnectOverlay(interruptedOverlayMessage);
  setStatus(interruptedOverlayMessage);
  scheduleReconnectStep(
    sequenceId,
    initialDelay,
    () => runIceReconnectAttempt(sequenceId, 0),
    { showBackoffDetail: false },
  );
}

function startImmediateFullReconnect(reason = "Manual reconnect requested") {
  clearCameraStartupTimer();
  finishCameraStartupOverlay({ keepOverlay: true });
  finishViewerStartupOverlay({ keepOverlay: true });
  cancelReconnectSequence();
  const sequenceId = reconnectSequenceId;
  showReconnectOverlay("Full reconnect 1", reason);
  scheduleFullReconnectAttempt(sequenceId, 0);
}

function sessionRequiresReconnect(session = currentSession) {
  if (!session?.pc) {
    return true;
  }

  return (
    ["disconnected", "failed", "closed"].includes(session.pc.connectionState) ||
    ["disconnected", "failed", "closed"].includes(session.pc.iceConnectionState)
  );
}

function reportError(error) {
  console.error(error);
  setStatus(`Error: ${error.message}`);
}

async function copyBlock(sourceId, button) {
  const content = document.getElementById(sourceId)?.textContent ?? "";
  if (!content) {
    return;
  }

  const originalText = button.textContent;

  try {
    await navigator.clipboard.writeText(content);
    button.textContent = "Copied";
  } catch (error) {
    console.error(error);
    button.textContent = "Copy failed";
  }

  window.setTimeout(() => {
    button.textContent = originalText;
  }, 1200);
}

function newMediaSection(mLine) {
  const parts = mLine.trim().split(/\s+/);
  return {
    mLine,
    kind: parts[0] || "media",
    port: parts[1] || "",
    protocol: parts[2] || "",
    payloads: parts.slice(3),
    connection: "",
    direction: "",
    mid: "",
    msid: "",
    iceUfrag: "",
    icePwd: "",
    setup: "",
    fingerprint: "",
    extmaps: [],
    candidates: [],
    codecs: new Map(),
    ssrcs: [],
    attributes: [],
  };
}

function ensureCodec(section, payloadType) {
  if (!section.codecs.has(payloadType)) {
    section.codecs.set(payloadType, {
      payloadType,
      rtpmap: "",
      fmtp: [],
      rtcpFb: [],
    });
  }

  return section.codecs.get(payloadType);
}

function parseCandidateLine(line) {
  const raw = line.replace(/^candidate:/, "");
  const parts = raw.trim().split(/\s+/);
  const candidate = {
    raw: line,
    foundation: parts[0] || "",
    component: parts[1] || "",
    protocol: parts[2] || "",
    priority: parts[3] || "",
    address: parts[4] || "",
    port: parts[5] || "",
    type: "",
    relatedAddress: "",
    relatedPort: "",
    tcpType: "",
  };

  for (let index = 6; index < parts.length; index += 2) {
    const key = parts[index];
    const value = parts[index + 1] || "";

    if (key === "typ") {
      candidate.type = value;
    } else if (key === "raddr") {
      candidate.relatedAddress = value;
    } else if (key === "rport") {
      candidate.relatedPort = value;
    } else if (key === "tcptype") {
      candidate.tcpType = value;
    }
  }

  return candidate;
}

function parseSdp(sdpText) {
  const parsed = {
    session: {
      version: "",
      origin: "",
      sessionName: "",
      timing: "",
      groups: [],
      msidSemantic: [],
      fingerprints: [],
      iceOptions: [],
      attributes: [],
    },
    mediaSections: [],
  };

  let currentSection = null;
  const lines = sdpText.split(/\r?\n/).map((line) => line.trim()).filter(Boolean);

  for (const line of lines) {
    if (line.startsWith("m=")) {
      currentSection = newMediaSection(line.slice(2));
      parsed.mediaSections.push(currentSection);
      continue;
    }

    if (line.startsWith("v=")) {
      parsed.session.version = line.slice(2);
      continue;
    }

    if (line.startsWith("o=")) {
      parsed.session.origin = line.slice(2);
      continue;
    }

    if (line.startsWith("s=")) {
      parsed.session.sessionName = line.slice(2);
      continue;
    }

    if (line.startsWith("t=")) {
      parsed.session.timing = line.slice(2);
      continue;
    }

    if (line.startsWith("c=") && currentSection) {
      currentSection.connection = line.slice(2);
      continue;
    }

    if (!line.startsWith("a=")) {
      if (currentSection) {
        currentSection.attributes.push(line);
      } else {
        parsed.session.attributes.push(line);
      }
      continue;
    }

    const attribute = line.slice(2);
    const target = currentSection ?? parsed.session;

    if (!currentSection) {
      if (attribute.startsWith("group:")) {
        parsed.session.groups.push(attribute.slice("group:".length));
        continue;
      }

      if (attribute.startsWith("msid-semantic:")) {
        parsed.session.msidSemantic.push(attribute.slice("msid-semantic:".length));
        continue;
      }

      if (attribute.startsWith("fingerprint:")) {
        parsed.session.fingerprints.push(attribute.slice("fingerprint:".length));
        continue;
      }

      if (attribute.startsWith("ice-options:")) {
        parsed.session.iceOptions.push(attribute.slice("ice-options:".length));
        continue;
      }

      parsed.session.attributes.push(attribute);
      continue;
    }

    if (["sendrecv", "recvonly", "sendonly", "inactive"].includes(attribute)) {
      currentSection.direction = attribute;
      continue;
    }

    if (attribute.startsWith("mid:")) {
      currentSection.mid = attribute.slice("mid:".length);
      continue;
    }

    if (attribute.startsWith("msid:")) {
      currentSection.msid = attribute.slice("msid:".length);
      continue;
    }

    if (attribute.startsWith("ice-ufrag:")) {
      currentSection.iceUfrag = attribute.slice("ice-ufrag:".length);
      continue;
    }

    if (attribute.startsWith("ice-pwd:")) {
      currentSection.icePwd = attribute.slice("ice-pwd:".length);
      continue;
    }

    if (attribute.startsWith("setup:")) {
      currentSection.setup = attribute.slice("setup:".length);
      continue;
    }

    if (attribute.startsWith("fingerprint:")) {
      currentSection.fingerprint = attribute.slice("fingerprint:".length);
      continue;
    }

    if (attribute.startsWith("extmap:")) {
      currentSection.extmaps.push(attribute.slice("extmap:".length));
      continue;
    }

    if (attribute.startsWith("candidate:")) {
      currentSection.candidates.push(parseCandidateLine(attribute));
      continue;
    }

    if (attribute.startsWith("rtpmap:")) {
      const [payloadType, description = ""] = attribute.slice("rtpmap:".length).split(/\s+/, 2);
      ensureCodec(currentSection, payloadType).rtpmap = description;
      continue;
    }

    if (attribute.startsWith("fmtp:")) {
      const [payloadType, description = ""] = attribute.slice("fmtp:".length).split(/\s+/, 2);
      ensureCodec(currentSection, payloadType).fmtp.push(description);
      continue;
    }

    if (attribute.startsWith("rtcp-fb:")) {
      const [payloadType, ...rest] = attribute.slice("rtcp-fb:".length).split(/\s+/);
      ensureCodec(currentSection, payloadType).rtcpFb.push(rest.join(" "));
      continue;
    }

    if (attribute.startsWith("ssrc:")) {
      currentSection.ssrcs.push(attribute.slice("ssrc:".length));
      continue;
    }

    target.attributes.push(attribute);
  }

  return parsed;
}

function renderList(items, formatter = (item) => escapeHtml(item)) {
  if (!items.length) {
    return `<span class="sdp-inline-code">none</span>`;
  }

  return `<ul class="sdp-list">${items.map((item) => `<li>${formatter(item)}</li>`).join("")}</ul>`;
}

function renderRow(label, content) {
  return `<dt>${escapeHtml(label)}</dt><dd>${content}</dd>`;
}

function renderCodec(codec) {
  const extras = [...codec.fmtp.map((item) => `fmtp ${item}`), ...codec.rtcpFb.map((item) => `rtcp-fb ${item}`)];
  const base = codec.rtpmap ? `${codec.payloadType} ${codec.rtpmap}` : codec.payloadType;
  return extras.length === 0
    ? `<span class="sdp-inline-code">${escapeHtml(base)}</span>`
    : `${escapeHtml(base)}${renderList(extras)}`;
}

function renderCandidate(candidate) {
  const related = candidate.relatedAddress
    ? ` via ${candidate.relatedAddress}:${candidate.relatedPort || "?"}`
    : "";
  const tcpType = candidate.tcpType ? ` ${candidate.tcpType}` : "";
  return `${escapeHtml(candidate.type || "candidate")} ${escapeHtml(candidate.protocol.toLowerCase())} ${escapeHtml(candidate.address)}:${escapeHtml(candidate.port)}${related}${tcpType}`;
}

function renderReadableSdp(parsed, title) {
  const sessionRows = [
    renderRow("Version", `<span class="sdp-inline-code">${escapeHtml(parsed.session.version || "0")}</span>`),
    renderRow("Origin", escapeHtml(parsed.session.origin || "n/a")),
    renderRow("Session", escapeHtml(parsed.session.sessionName || "n/a")),
    renderRow("Timing", escapeHtml(parsed.session.timing || "n/a")),
    renderRow("Groups", renderList(parsed.session.groups)),
    renderRow("MSID semantic", renderList(parsed.session.msidSemantic)),
    renderRow("Fingerprints", renderList(parsed.session.fingerprints)),
    renderRow("ICE options", renderList(parsed.session.iceOptions)),
    renderRow("Other attrs", renderList(parsed.session.attributes)),
  ].join("");

  const mediaMarkup = parsed.mediaSections.map((section, index) => {
    const codecList = [...section.codecs.values()];
    return `
      <div class="sdp-card">
        <p class="sdp-card-title sdp-media-title">Media ${index + 1}: ${escapeHtml(section.kind)}</p>
        <dl class="sdp-grid">
          ${renderRow("m-line", `<span class="sdp-inline-code">${escapeHtml(section.mLine)}</span>`)}
          ${renderRow("MID", escapeHtml(section.mid || "n/a"))}
          ${renderRow("Direction", escapeHtml(section.direction || "n/a"))}
          ${renderRow("Transport", escapeHtml(section.protocol || "n/a"))}
          ${renderRow("Payloads", section.payloads.length ? renderList(section.payloads.map((item) => `<span class="sdp-inline-code">${escapeHtml(item)}</span>`), (item) => item) : `<span class="sdp-inline-code">none</span>`)}
          ${renderRow("Connection", escapeHtml(section.connection || "n/a"))}
          ${renderRow("MSID", escapeHtml(section.msid || "n/a"))}
          ${renderRow("ICE", escapeHtml(`${section.iceUfrag || "n/a"} / ${section.icePwd || "n/a"}`))}
          ${renderRow("Setup", escapeHtml(section.setup || "n/a"))}
          ${renderRow("Fingerprint", escapeHtml(section.fingerprint || "n/a"))}
          ${renderRow("Codecs", codecList.length ? renderList(codecList, renderCodec) : `<span class="sdp-inline-code">none</span>`)}
          ${renderRow("Extmaps", renderList(section.extmaps))}
          ${renderRow("Candidates", section.candidates.length ? renderList(section.candidates, renderCandidate) : `<span class="sdp-inline-code">none</span>`)}
          ${renderRow("SSRCs", renderList(section.ssrcs))}
          ${renderRow("Other attrs", renderList(section.attributes))}
        </dl>
      </div>
    `;
  }).join("");

  return `
    <div class="sdp-card">
      <p class="sdp-card-title">${escapeHtml(title)} overview</p>
      <dl class="sdp-grid">${sessionRows}</dl>
    </div>
    ${mediaMarkup || `
      <div class="sdp-card">
        <p class="sdp-card-title">No media sections</p>
      </div>
    `}
  `;
}

function updateSdpViews(kind, sdpText) {
  const rawTarget = kind === "offer" ? debugRefs.offerSdp : debugRefs.answerSdp;
  const readableTarget = kind === "offer" ? debugRefs.offerReadable : debugRefs.answerReadable;
  const title = kind === "offer" ? "Browser offer" : "Server answer";

  rawTarget.textContent = sdpText || "Missing SDP";

  if (!sdpText) {
    readableTarget.innerHTML = `
      <div class="sdp-card">
        <p class="sdp-card-title">Missing SDP</p>
      </div>
    `;
    return;
  }

  readableTarget.innerHTML = renderReadableSdp(parseSdp(sdpText), title);
}

async function refreshStats(pc) {
  const stats = await pc.getStats();
  const role = currentSession?.role || requestedRole;
  let inboundVideo = null;
  let outboundVideo = null;
  let codec = null;
  let selectedPair = null;
  let roundTripTime = null;
  let localCandidate = null;
  let remoteCandidate = null;
  let selectedPairId = null;

  stats.forEach((report) => {
    if (report.type === "transport" && report.selectedCandidatePairId) {
      selectedPairId = report.selectedCandidatePairId;
    }
  });

  stats.forEach((report) => {
    if (report.type === "inbound-rtp" && report.kind === "video" && !report.isRemote) {
      inboundVideo = report;
    }

    if (report.type === "outbound-rtp" && report.kind === "video" && !report.isRemote) {
      outboundVideo = report;
    }
  });

  if (selectedPairId && stats.has(selectedPairId)) {
    selectedPair = stats.get(selectedPairId);
  }

  if (!selectedPair) {
    stats.forEach((report) => {
      if (report.type === "candidate-pair" && (report.selected || (report.nominated && report.state === "succeeded"))) {
        selectedPair = report;
      }
    });
  }

  if (selectedPair?.localCandidateId && stats.has(selectedPair.localCandidateId)) {
    localCandidate = stats.get(selectedPair.localCandidateId);
  }

  if (selectedPair?.remoteCandidateId && stats.has(selectedPair.remoteCandidateId)) {
    remoteCandidate = stats.get(selectedPair.remoteCandidateId);
  }

  const mediaReport = role === cameraRole ? outboundVideo : inboundVideo;

  if (mediaReport?.codecId && stats.has(mediaReport.codecId)) {
    codec = stats.get(mediaReport.codecId);
  }

  if (selectedPair && Number.isFinite(selectedPair.currentRoundTripTime)) {
    roundTripTime = selectedPair.currentRoundTripTime;
  }

  if (mediaReport) {
    const bytes = role === cameraRole ? mediaReport.bytesSent ?? 0 : mediaReport.bytesReceived ?? 0;
    const frames = role === cameraRole
      ? mediaReport.framesSent ?? 0
      : mediaReport.framesDecoded ?? mediaReport.framesReceived ?? 0;

    setMetric("codecInfo", codec?.mimeType || "video");
    setMetric("bytesReceived", formatBytes(bytes));
    setMetric("framesDecoded", String(frames));
    setMetric("packetsLost", String(mediaReport.packetsLost ?? 0));
    setMetric("timingInfo", formatTiming(role === cameraRole ? null : mediaReport.jitter, roundTripTime));

    const localTrack = localCameraStream ? localCameraStream.getVideoTracks()[0] : null;
    const localTrackSettings = localTrack?.getSettings?.() ?? {};
    const frameWidth = remoteVideo.videoWidth || mediaReport.frameWidth || localTrackSettings.width;
    const frameHeight = remoteVideo.videoHeight || mediaReport.frameHeight || localTrackSettings.height;
    setMetric(
      "videoDimensions",
      frameWidth && frameHeight ? `${frameWidth} x ${frameHeight}` : "pending",
    );

    if (lastStatsSnapshot) {
      setMetric(
        "bitrate",
        formatBitrate(
          bytes - lastStatsSnapshot.bytesReceived,
          performance.now() - lastStatsSnapshot.timestamp,
        ),
      );
    }

    if (role !== cameraRole) {
      const previousFrames = streamStallState.lastFramesDecoded;
      const previousDisplayedFrames = streamStallState.lastDisplayedFrames;
      pollPlaybackFrames();

      if (awaitingFirstStartupFrame()) {
        streamStallState.stalledIntervals = 0;
      } else {
        const displayedFramesProgressed = previousDisplayedFrames !== null
          ? displayedFrameCount > previousDisplayedFrames
          : hasDisplayedFrameProgress();
        const decodedFramesProgressed = previousFrames === null || frames > (previousFrames ?? 0);
        const progressed = displayedFramesProgressed || (!hasDisplayedFrameProgress() && decodedFramesProgressed);

        if (progressed) {
          streamStallState.stalledIntervals = 0;
          streamStallState.hasSeenProgress = true;
          if (!remoteVideo.paused) {
            hideResumeOverlayIfReady();
          }
        } else if (streamStallState.hasSeenProgress) {
          streamStallState.stalledIntervals += 1;
          if (streamStallState.stalledIntervals >= 3) {
            handleStalledPlayback("Connection Stalled");
          }
        }
      }

      streamStallState.lastFramesDecoded = frames;
      streamStallState.lastDisplayedFrames = displayedFrameCount;
    }

    lastStatsSnapshot = {
      bytesReceived: bytes,
      timestamp: performance.now(),
    };
  }

  if (selectedPair) {
    setMetric(
      "candidatePairInfo",
      `${formatCandidate(localCandidate)} -> ${formatCandidate(remoteCandidate)}`,
    );
  }
}

function installDebugUI() {
  if (debugUiInstalled) {
    return;
  }

  debugUiInstalled = true;
  installFrameIndicatorMonitor();
  setDebugPanelOpen(false);
  activatePanel("overview");
  activateSdpMode("offer", "readable");
  activateSdpMode("answer", "readable");
  updateEyeButton();

  panelButtons.forEach((button) => {
    button.addEventListener("click", () => {
      activatePanel(button.dataset.panelTarget);
    });
  });

  sdpModeButtons.forEach((button) => {
    button.addEventListener("click", () => {
      activateSdpMode(button.dataset.sdpTarget, button.dataset.sdpMode);
    });
  });

  copyButtons.forEach((button) => {
    button.addEventListener("click", () => {
      copyBlock(button.dataset.copySource, button);
    });
  });

  window.addEventListener("resize", syncDebugPanelSpace);

  channelButtons.forEach((button) => {
    button.addEventListener("click", () => {
      if (requestedRole === cameraRole) {
        return;
      }

      const slot = normalizeChannelSlot(button.dataset.channelSlot);
      const slotState = channelSlotStates.find((candidate) => candidate.slot === slot);
      if (!slotState?.occupied || selectedChannelSlot === slot) {
        return;
      }

      selectedChannelSlot = slot;
      updateChannelButtons();
      setStatus(`Switching to channel ${slot}...`);
      sendSelectedChannelChange(slot);
    });
  });

  debugToggle.addEventListener("click", () => {
    setDebugPanelOpen(!debugPanelOpen);
  });

  eyeButton.addEventListener("click", async () => {
    const nextRole = requestedRole === cameraRole ? viewerRole : cameraRole;

    try {
      if (nextRole === cameraRole) {
        finishViewerStartupOverlay({ keepOverlay: true });
        startCameraStartupOverlay();
        const slotState = await fetchCameraSlots();
        if (!hasAvailableCameraSlot(slotState.slots)) {
          finishCameraStartupOverlay();
          showCameraSlotLimitReached();
          return;
        }

        await ensureLocalCameraStream();
      } else {
        finishCameraStartupOverlay();
        startViewerStartupOverlay();
      }

      requestedRole = nextRole;
      if (nextRole !== cameraRole) {
        assignedCameraSlot = 0;
      }
      updateEyeButton();
      connectToServer(
        nextRole === cameraRole ? "Activating camera broadcast..." : "Switching back to viewer mode...",
        nextRole,
        { preserveRecoveryOverlay: nextRole === cameraRole },
      ).catch((error) => {
        if (error.message !== supersededConnectionMessage) {
          if (nextRole === cameraRole && cameraSlotsFull(error)) {
            finishCameraStartupOverlay();
            requestedRole = viewerRole;
            assignedCameraSlot = 0;
            updateEyeButton();
            showCameraSlotLimitReached();
            connectToServer("Returning to viewer mode...", viewerRole).catch((viewerError) => {
              if (viewerError.message !== supersededConnectionMessage) {
                reportError(viewerError);
              }
            });
            return;
          }
          if (nextRole === cameraRole) {
            appendLog(`Camera startup failed: ${error.message}. Starting full reconnect.`);
            startImmediateFullReconnect();
            return;
          }
          finishViewerStartupOverlay({ keepOverlay: true });
          reportError(error);
        }
      });
    } catch (error) {
      finishCameraStartupOverlay();
      const message = describeCameraError(error);
      appendLog(message);
      setStatus(message);
    }
  });

  resumeOverlay.addEventListener("click", async () => {
    if (overlayRecoveryMode !== "reconnect") {
      const resumed = await attemptResumePlayback("Manual play button pressed", false);
      if (resumed && !remoteVideo.paused) {
        return;
      }

      startImmediateFullReconnect();
      return;
    }

    startImmediateFullReconnect();
  });

  remoteVideo.addEventListener("playing", () => {
    pollPlaybackFrames();
    hideResumeOverlayIfReady();
  });

  remoteVideo.addEventListener("pause", () => {
    if (!remoteVideo.srcObject || document.visibilityState === "hidden") {
      return;
    }

    if (requestedRole === cameraRole && !sessionRequiresReconnect()) {
      return;
    }

    if (sessionRequiresReconnect()) {
      showReconnectOverlay(resumeOverlayMessage || interruptedOverlayMessage, "Remote video paused");
      return;
    }

    scheduleResumeCheck("Remote video paused");
  });

  remoteVideo.addEventListener("loadeddata", () => {
    pollPlaybackFrames();
    hideResumeOverlayIfReady();
  });

  remoteVideo.addEventListener("emptied", () => {
    showReconnectOverlay(resumeOverlayMessage || interruptedOverlayMessage, "Remote video emptied");
  });

  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") {
      if (overlayRecoveryMode === "reconnect" || sessionRequiresReconnect()) {
        showReconnectOverlay(resumeOverlayMessage || interruptedOverlayMessage, "Page became visible again");
        return;
      }

      scheduleResumeCheck("Page became visible again", 180);
      return;
    }

    resumeOverlay.hidden = true;
  });

  window.addEventListener("pageshow", () => {
    if (overlayRecoveryMode === "reconnect" || sessionRequiresReconnect()) {
      showReconnectOverlay(resumeOverlayMessage || interruptedOverlayMessage, "Page restored");
      return;
    }

    scheduleResumeCheck("Page restored", 180);
  });
}

async function connectToServer(
  reason = "Connecting to Go/Pion server...",
  role = requestedRole,
  { preserveRecoveryOverlay = false, sessionId = createSessionId() } = {},
) {
  if (!preserveRecoveryOverlay) {
    cancelReconnectSequence();
  }

  const nextRole = role === cameraRole ? cameraRole : viewerRole;
  const preparedCameraStream = nextRole === cameraRole ? await ensureLocalCameraStream() : null;
  const token = ++activeSessionToken;
  const previousSession = currentSession;
  currentSession = { token, pc: null, statsTimer: null, role: nextRole, sessionId, controlChannel: null };

  disposeSession(previousSession);
  if (nextRole !== cameraRole) {
    releaseLocalCamera();
    setScreenStream(null);
  }

  resetStreamStallState();
  resetCameraAckState();
  resetLiveMetrics();
  lastStatsSnapshot = null;
  if (!preserveRecoveryOverlay && !awaitingFirstStartupFrame()) {
    hideResumeOverlay();
  }
  if (nextRole === cameraRole) {
    armCameraStartupTimer(token);
  } else {
    clearCameraStartupTimer();
  }
  setStatus(reason);

  const pc = new RTCPeerConnection({ iceServers: [] });
  currentSession.pc = pc;
  installControlChannel(pc, token, nextRole);
  if (nextRole === cameraRole) {
    setScreenStream(preparedCameraStream, { mirror: true });
    setMetric("remoteTrackInfo", "local camera preview");
    const [cameraTrack] = preparedCameraStream.getVideoTracks();
    if (!cameraTrack) {
      throw new Error("Camera stream is missing a video track.");
    }

    const cameraTransceiver = pc.addTransceiver(cameraTrack, {
      direction: "sendonly",
      streams: [preparedCameraStream],
    });

    if (typeof cameraTransceiver.setCodecPreferences !== "function") {
      throw new Error("This browser cannot force VP8 camera negotiation.");
    }

    cameraTransceiver.setCodecPreferences(forcedCameraVP8Codecs());
  } else {
    pc.addTransceiver("video", { direction: "recvonly" });
  }

  updateStaticState(pc);
  setRoleMetric(nextRole);
  setSourceMetric(nextRole === cameraRole ? "Local camera" : "Server feed");

  pc.addEventListener("signalingstatechange", () => {
    if (!isCurrentSession(token)) {
      return;
    }

    updateStaticState(pc);
    appendLog(`Signaling state -> ${pc.signalingState}`);
  });

  pc.addEventListener("icegatheringstatechange", () => {
    if (!isCurrentSession(token)) {
      return;
    }

    updateStaticState(pc);
    appendLog(`ICE gathering -> ${pc.iceGatheringState}`);
  });

  pc.addEventListener("iceconnectionstatechange", () => {
    if (!isCurrentSession(token)) {
      return;
    }

    updateStaticState(pc);
    appendLog(`ICE connection -> ${pc.iceConnectionState}`);

    if (["disconnected", "failed", "closed"].includes(pc.iceConnectionState)) {
      clearSessionStats(currentSession);
      startReconnectSequence(pc.iceConnectionState === "disconnected" ? iceReconnectDelaysMs[0] : 0);
      return;
    }

    if (["connected", "completed"].includes(pc.iceConnectionState)) {
      cancelReconnectSequence();
      ensureSessionStatsTimer(currentSession);
      if (!awaitingFirstStartupFrame()) {
        hideResumeOverlayIfReady();
        scheduleResumeCheck(`ICE connection -> ${pc.iceConnectionState}`, 120);
      }
    }
  });

  pc.addEventListener("connectionstatechange", () => {
    if (!isCurrentSession(token)) {
      return;
    }

    updateStaticState(pc);
    appendLog(`Peer connection -> ${pc.connectionState}`);

    if (["disconnected", "failed", "closed"].includes(pc.connectionState)) {
      clearSessionStats(currentSession);
      startReconnectSequence(pc.connectionState === "disconnected" ? iceReconnectDelaysMs[0] : 0);
      return;
    }

    if (pc.connectionState === "connected") {
      cancelReconnectSequence();
      ensureSessionStatsTimer(currentSession);
      if (!awaitingFirstStartupFrame()) {
        if (nextRole === cameraRole) {
          hideResumeOverlayIfReady();
        } else {
          scheduleResumeCheck("Peer connection -> connected", 120);
        }
      }
    }
  });

  pc.addEventListener("track", async (event) => {
    if (!isCurrentSession(token)) {
      return;
    }

    const [streamFromServer] = event.streams;
    setScreenStream(streamFromServer ?? new MediaStream([event.track]));
    resetStreamStallState();
    setMetric("remoteTrackInfo", `${event.track.kind} ${event.track.id}`);
    setSourceMetric("Server feed");
    appendLog(`Remote track received: ${event.track.kind} ${event.track.id}`);

    event.track.addEventListener("mute", () => {
      if (!isCurrentSession(token)) {
        return;
      }

      showReconnectOverlay(resumeOverlayMessage || interruptedOverlayMessage, "Remote track muted");
    });

    event.track.addEventListener("unmute", () => {
      if (!isCurrentSession(token)) {
        return;
      }

      scheduleResumeCheck("Remote track unmuted", 120);
    });

    event.track.addEventListener("ended", () => {
      if (!isCurrentSession(token)) {
        return;
      }

      if (sessionRequiresReconnect()) {
        clearSessionStats(currentSession);
        showReconnectOverlay(resumeOverlayMessage || interruptedOverlayMessage, "Remote track ended");
        return;
      }

      ensureSessionStatsTimer(currentSession);
      setStatus("Waiting for server video...");
    });

    await attemptResumePlayback("Remote track received");
    setStatus("Remote server video is live.");
  });

  try {
    await negotiateSession(currentSession);
    if (nextRole === cameraRole) {
      setStatus("Camera mode active. Sending local preview to the server.");
      setMetric("remoteTrackInfo", "local camera uplink");
      if (!preserveRecoveryOverlay && !awaitingFirstStartupFrame()) {
        hideResumeOverlayIfReady();
      }
    } else {
      setStatus("Negotiation complete. Waiting for server video...");
    }

    ensureSessionStatsTimer(currentSession);
  } catch (error) {
    disposeSession(isCurrentSession(token) ? currentSession : { pc });

    if (nextRole === viewerRole) {
      finishViewerStartupOverlay({ keepOverlay: true });
    }

    if (isCurrentSession(token) && error.message !== supersededConnectionMessage && !preserveRecoveryOverlay) {
      showReconnectOverlay(`Connection setup failed: ${error.message}`);
    }

    throw error;
  }

  return currentSession;
}

installDebugUI();
updateEyeButton();
startViewerStartupOverlay();
connectToServer("Starting WebRTC session...").catch((error) => {
  if (error.message !== supersededConnectionMessage) {
    finishViewerStartupOverlay({ keepOverlay: true });
    reportError(error);
  }
});
