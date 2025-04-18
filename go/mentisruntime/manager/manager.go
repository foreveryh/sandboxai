package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io" // Added for reading response body
	"log/slog"
	"net/http"
	"os" // Add this import for environment variable access
	"strings" // Added for IP address check
	"sync"
	"time" // Added for context timeout

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat" // Import for nat.PortSet
	"github.com/google/uuid"
	"github.com/foreveryh/sandboxai/go/mentisruntime/ws" // Import WebSocket Hub
)

type SandboxState struct {
	ContainerID string
	AgentURL    string // e.g., http://<container_ip>:<agent_port>
	IsRunning   bool
	// Add other relevant state fields
}

type SandboxManager struct {
	mu           sync.RWMutex
	sandboxes    map[string]*SandboxState // Map sandboxID to its state
	httpClient   *http.Client
	logger       *slog.Logger
	dockerClient *client.Client // Docker client for container operations
	hub          *ws.Hub          // WebSocket Hub for broadcasting observations
	scope        string           // Scope for managing containers
}

// NewSandboxManager creates a new SandboxManager.
func NewSandboxManager(ctx context.Context, dockerClient *client.Client, hub *ws.Hub, logger *slog.Logger, scope string) (*SandboxManager, error) {
	m := &SandboxManager{
		sandboxes:    make(map[string]*SandboxState),
		httpClient:   &http.Client{}, // Configure as needed
		logger:       logger.With("component", "sandbox-manager"),
		dockerClient: dockerClient,
		hub:          hub,
		scope:        scope,
	}
	// TODO: Potentially discover existing sandboxes on startup?
	return m, nil
}

// SandboxExists checks if a sandbox with the given ID is known to the manager.
// This method implements the ws.SandboxChecker interface.
func (m *SandboxManager) SandboxExists(ctx context.Context, sandboxID string) (bool, error) {
	m.mu.RLock()
	_, exists := m.sandboxes[sandboxID]
	m.mu.RUnlock()
	// In this basic implementation, we don't return an error, just existence.
	// A more complex implementation might check Docker or other sources.
	return exists, nil
}

// InitiateAction starts an action (shell or ipython) asynchronously.
// It generates an action ID, validates the sandbox state, launches a goroutine
// for execution, and returns the action ID immediately.
func (m *SandboxManager) InitiateAction(ctx context.Context, sandboxID string, actionType string, payload map[string]interface{}) (string, error) {
	m.mu.RLock()
	state, exists := m.sandboxes[sandboxID]
	m.mu.RUnlock()

	if !exists || !state.IsRunning {
		return "", fmt.Errorf("sandbox %s not found or not running", sandboxID)
	}

	actionID := uuid.NewString()

	// Construct the request body for the internal agent
	requestPayload := map[string]interface{}{
		"action_id": actionID,
	}
	for k, v := range payload {
		requestPayload[k] = v // Copy original payload (command, code, etc.)
	}

	requestBody, err := json.Marshal(requestPayload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body for agent: %w", err)
	}

	var agentURL string
	switch actionType {
	case "shell":
		agentURL = fmt.Sprintf("%s/tools:run_shell_command", state.AgentURL) // Corrected path
	case "ipython":
		agentURL = fmt.Sprintf("%s/tools:run_ipython_cell", state.AgentURL) // Corrected path
	default:
		return "", fmt.Errorf("unsupported action type: %s", actionType)
	}

	// Launch the goroutine to handle the actual execution and streaming
	go m.handleActionExecution(context.Background(), sandboxID, actionID, agentURL, requestBody, actionType)

	m.logger.Info("Action initiated", "sandboxID", sandboxID, "actionID", actionID, "actionType", actionType)
	return actionID, nil // Return immediately
}

// Observation types (Placeholders - define properly later)
type Observation struct {
	Type     string      `json:"type"` // e.g., "start", "stream", "error", "end"
	ActionID string      `json:"action_id"`
	Data     interface{} `json:"data,omitempty"`
}

type StartObservationData struct {
	// Add relevant start data if needed
}

type StreamObservationData struct {
	Stream string `json:"stream"` // "stdout" or "stderr"
	Line   string `json:"line"`
}

type ErrorObservationData struct {
	Error string `json:"error"`
}

type EndObservationData struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"` // Error message if exit code != 0
}

// AgentObservation defines the structure expected from the agent's streaming response lines.
// This allows the manager to understand structured messages like results.
type AgentObservation struct {
	Type     string          `json:"type"` // e.g., "stream", "result"
	Stream   string          `json:"stream,omitempty"` // "stdout", "stderr"
	Line     string          `json:"line,omitempty"`
	ExitCode *int            `json:"exit_code,omitempty"` // Use pointer to distinguish 0 from unset
	Error    string          `json:"error,omitempty"`
}

// handleActionExecution runs in a goroutine to execute the action via the internal agent.
// It only handles the initial request and immediate HTTP errors.
// Subsequent observations (stream, result) are handled by ReceiveInternalObservation.
func (m *SandboxManager) handleActionExecution(ctx context.Context, sandboxID, actionID, agentURL string, requestBody []byte, actionType string) {
	// Send StartObservation immediately via the Hub
	m.pushObservation(sandboxID, actionID, "start", StartObservationData{})

	req, err := http.NewRequestWithContext(ctx, "POST", agentURL, bytes.NewReader(requestBody))
	if err != nil {
		errMsg := fmt.Sprintf("Failed to create request to agent: %v", err)
		m.pushErrorObservation(sandboxID, actionID, errMsg)
		m.pushObservation(sandboxID, actionID, "end", EndObservationData{ExitCode: -1, Error: errMsg})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// We don't strictly need Accept header anymore if we don't read the body for observations
	// req.Header.Set("Accept", "application/x-ndjson") 

	resp, err := m.httpClient.Do(req)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to execute action request via agent: %v", err)
		m.pushErrorObservation(sandboxID, actionID, errMsg)
		m.pushObservation(sandboxID, actionID, "end", EndObservationData{ExitCode: -1, Error: errMsg})
		return
	}
	defer resp.Body.Close()

	// Handle only immediate HTTP errors from the agent
	if resp.StatusCode >= 400 {
		bodyBytes, readErr := io.ReadAll(resp.Body)
		errorMsg := fmt.Sprintf("Agent returned error status %d", resp.StatusCode)
		if readErr == nil && len(bodyBytes) > 0 {
			errorMsg += fmt.Sprintf(": %s", string(bodyBytes))
		} else if readErr != nil {
			errorMsg += fmt.Sprintf(" (failed to read error body: %v)", readErr)
		}
		m.pushErrorObservation(sandboxID, actionID, errorMsg)
		m.pushObservation(sandboxID, actionID, "end", EndObservationData{ExitCode: -1, Error: errorMsg})
		return
	}

	// If status code is OK (e.g., 200, 202), the request was accepted by the agent.
	// Log this success and exit the goroutine.
	// The agent will now asynchronously send observations via the /internal/observations endpoint.
	m.logger.Info("Action request successfully sent to agent", "sandboxID", sandboxID, "actionID", actionID, "agentURL", agentURL, "statusCode", resp.StatusCode)

	// DO NOT read resp.Body here for observations.
	// Let ReceiveInternalObservation handle stream/result/end logic based on pushed data.
}

// pushObservation formats and sends an observation via the hub.
func (m *SandboxManager) pushObservation(sandboxID, actionID, obsType string, data interface{}) {
	obs := Observation{
		Type:     obsType,
		ActionID: actionID,
		Data:     data,
	}

	jsonData, err := json.Marshal(obs)
	if err != nil {
		m.logger.Error("Failed to marshal observation", "error", err, "sandboxID", sandboxID, "actionID", actionID, "type", obsType)
		return
	}

	m.logger.Debug("Pushing observation via Hub", "sandboxID", sandboxID, "actionID", actionID, "type", obsType, "size", len(jsonData))
	// Send via Hub
	m.hub.SubmitBroadcast(sandboxID, jsonData)
}

// pushErrorObservation formats and sends an error observation.
func (m *SandboxManager) pushErrorObservation(sandboxID, actionID, errorMsg string) {
	m.logger.Error("Action error occurred", "sandboxID", sandboxID, "actionID", actionID, "error", errorMsg)
	m.pushObservation(sandboxID, actionID, "error", ErrorObservationData{Error: errorMsg})
}

// --- Sandbox Lifecycle Management --- 

// CreateSandbox creates a new sandbox container.
// It pulls the necessary image, creates and starts the container,
// discovers its IP address, and stores its state.
func (m *SandboxManager) CreateSandbox(ctx context.Context /* options */) (string /* sandboxID */, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sandboxID := uuid.NewString() // Generate a unique ID
	
	// Get image name from environment variable or use default
	imageName := os.Getenv("BOX_IMAGE")
	if imageName == "" {
		imageName = "mentisai/sandboxai-box:latest" // Default if no environment variable set
	}
	m.logger.Debug("Using box image", "image", imageName)
	
	agentPort := "8000/tcp" // Default agent port inside the container - CHANGED FROM 9090

	m.logger.Info("Creating sandbox", "sandboxID", sandboxID, "image", imageName)

	// 1. Ensure image exists locally
	// Use a shorter timeout for image pull check/pull
	pullCtx, pullCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer pullCancel()

	// First check if image exists locally
	inspectCtx, inspectCancel := context.WithTimeout(ctx, 10*time.Second)
	defer inspectCancel()
	_, _, errInspect := m.dockerClient.ImageInspectWithRaw(inspectCtx, imageName)
	if errInspect == nil {
		// Image exists locally, no need to pull
		m.logger.Info("Image exists locally, skipping pull", "image", imageName)
	} else {
		// Try to pull the image only if it doesn't exist locally
		m.logger.Info("Image not found locally, attempting to pull", "image", imageName)
		out, err := m.dockerClient.ImagePull(pullCtx, imageName, image.PullOptions{})
		if err != nil {
			m.logger.Error("Failed to pull image", "image", imageName, "error", err)
			return "", fmt.Errorf("failed to pull image %s: %w", imageName, err)
		}
		// IMPORTANT: Block and drain the output to ensure the pull completes before proceeding.
		// Discard the output, but log errors if reading fails.
		defer out.Close()
		if _, err = io.Copy(io.Discard, out); err != nil {
			m.logger.Error("Failed reading image pull output", "image", imageName, "error", err)
			return "", fmt.Errorf("failed reading image pull output for %s: %w", imageName, err)
		}
		m.logger.Info("Image pull completed", "image", imageName)
	}

	// Add an explicit check after pulling to ensure the image exists locally
	inspectCtx2, inspectCancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer inspectCancel2()
	_, _, errInspect2 := m.dockerClient.ImageInspectWithRaw(inspectCtx2, imageName)
	if errInspect2 != nil {
		m.logger.Error("Image inspect failed after pull", "image", imageName, "error", errInspect2)
		// Attempt to pull again, maybe there was a transient issue?
		// For now, just return the error.
		return "", fmt.Errorf("image %s not found locally after pull attempt: %w", imageName, errInspect2)
	}
	m.logger.Info("Image confirmed to exist locally", "image", imageName)

	// 2. Create the container
	containerName := fmt.Sprintf("sandboxai-%s-%s", m.scope, sandboxID)
	labels := map[string]string{
		"sandboxai.scope": m.scope,
		"sandboxai.id":    sandboxID,
	}
	// Determine the host address Runtime is listening on, as seen from the container
	// Using host.docker.internal which works for Docker Desktop. Might need configuration for other environments.
	runtimeHost := "host.docker.internal" 
	// Get the port Runtime is listening on (assuming it's passed via env var or default)
	runtimePort := os.Getenv("SANDBOXAID_PORT")
	if runtimePort == "" {
		runtimePort = "5266" // Default port used in main.go
	}
	internalObservationURL := fmt.Sprintf("http://%s:%s/internal/observations/%s", runtimeHost, runtimePort, sandboxID)

	envVars := []string{
		fmt.Sprintf("SANDBOX_ID=%s", sandboxID),
		// Add other necessary env vars for the agent
		fmt.Sprintf("RUNTIME_OBSERVATION_URL=%s", internalObservationURL), // Add URL for agent to push observations
	}

	// Use a shorter timeout for container operations
	createCtx, createCancel := context.WithTimeout(ctx, 30*time.Second)
	defer createCancel()

	resp, err := m.dockerClient.ContainerCreate(
		createCtx,
		&container.Config{
			Image:        imageName,
			Labels:       labels,
			Env:          envVars,
			ExposedPorts: nat.PortSet{nat.Port(agentPort): struct{}{}}, // Expose agent port
			// Tty:          false, // Usually false for background services
			// OpenStdin:    false,
		},
		&container.HostConfig{
			// AutoRemove: true, // Automatically remove container when it exits
			// PortBindings: nat.PortMap{ // Example: Map to host port if needed
			//  agentPort: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: ""}}, // Empty HostPort for dynamic assignment
			// },
		},
		&network.NetworkingConfig{ // Default network is usually fine
		},
		nil, // Platform is usually nil
		containerName,
	)
	if err != nil {
		m.logger.Error("Failed to create container", "sandboxID", sandboxID, "name", containerName, "error", err)
		return "", fmt.Errorf("failed to create container: %w", err)
	}

	m.logger.Info("Container created", "sandboxID", sandboxID, "containerID", resp.ID, "name", containerName)

	// 3. Start the container
	startCtx, startCancel := context.WithTimeout(ctx, 15*time.Second)
	defer startCancel()
	if err := m.dockerClient.ContainerStart(startCtx, resp.ID, container.StartOptions{}); err != nil {
		m.logger.Error("Failed to start container", "sandboxID", sandboxID, "containerID", resp.ID, "error", err)
		// Attempt to remove the created container on start failure
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rmCancel()
		if rmErr := m.dockerClient.ContainerRemove(rmCtx, resp.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			m.logger.Error("Failed to remove container after start failure", "containerID", resp.ID, "removeError", rmErr)
		}
		return "", fmt.Errorf("failed to start container %s: %w", resp.ID, err)
	}

	// 4. Inspect the container to get its IP address on the default bridge network
	inspectCtx, inspectCancel = context.WithTimeout(ctx, 10*time.Second) // Changed := to =
	defer inspectCancel()
	inspectData, err := m.dockerClient.ContainerInspect(inspectCtx, resp.ID)
	if err != nil {
		m.logger.Error("Failed to inspect container after start", "sandboxID", sandboxID, "containerID", resp.ID, "error", err)
		// Consider stopping and removing the container here as well
		return "", fmt.Errorf("failed to inspect container %s: %w", resp.ID, err)
	}

	// Find IP address - assumes default bridge network
	var containerIP string
	if inspectData.NetworkSettings != nil && inspectData.NetworkSettings.Networks != nil {
		for _, netSettings := range inspectData.NetworkSettings.Networks {
			if netSettings.IPAddress != "" && !strings.HasPrefix(netSettings.IPAddress, "172.17.") { // Basic check, might need refinement
				containerIP = netSettings.IPAddress
				break
			}
		}
		// Fallback to the first available IP if specific one not found
		if containerIP == "" {
			for _, netSettings := range inspectData.NetworkSettings.Networks {
				if netSettings.IPAddress != "" {
					containerIP = netSettings.IPAddress
					break
				}
			}
		}
	}

	if containerIP == "" {
		m.logger.Error("Failed to find container IP address", "sandboxID", sandboxID, "containerID", resp.ID)
		// Consider stopping and removing the container
		return "", fmt.Errorf("failed to find IP address for container %s", resp.ID)
	}

	// 5. Construct Agent URL
	// Extract port number from agentPort string like "9090/tcp"
	portNum := strings.Split(agentPort, "/")[0]
	agentURL := fmt.Sprintf("http://%s:%s", containerIP, portNum)

	m.logger.Info("Sandbox container started successfully", "sandboxID", sandboxID, "containerID", resp.ID, "containerIP", containerIP, "agentURL", agentURL)

	// 6. Store the state
	state := &SandboxState{
		ContainerID: resp.ID,
		AgentURL:    agentURL,
		IsRunning:   true,
	}
	m.sandboxes[sandboxID] = state

	return sandboxID, nil
}

// DeleteSandbox stops and removes a sandbox container.
// TODO: Implement the actual container removal logic using m.dockerClient.
func (m *SandboxManager) DeleteSandbox(ctx context.Context, sandboxID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.sandboxes[sandboxID]
	if !exists {
		return fmt.Errorf("sandbox %s not found", sandboxID)
	}

	m.logger.Info("Deleting sandbox", "sandboxID", sandboxID, "containerID", state.ContainerID)

	// --- Placeholder Logic --- 
	// Replace with actual Docker interaction (stop, remove container)
	// 1. Stop the container
	// Use a reasonable timeout for stop operation
	stopTimeout := 10 * time.Second // 10 seconds
	stopCtx, stopCancel := context.WithTimeout(ctx, stopTimeout)
	defer stopCancel()

	m.logger.Debug("Stopping container", "containerID", state.ContainerID, "timeout", stopTimeout)
	stopTimeoutInt := int(stopTimeout.Seconds())
	if err := m.dockerClient.ContainerStop(stopCtx, state.ContainerID, container.StopOptions{Timeout: &stopTimeoutInt}); err != nil {
		// Log the error but proceed to attempt removal anyway
		m.logger.Error("Failed to stop container (proceeding with removal attempt)", "sandboxID", sandboxID, "containerID", state.ContainerID, "error", err)
		// Check if the error indicates the container is already stopped or gone
		if !client.IsErrNotFound(err) && !strings.Contains(err.Error(), "is already stopped") {
			// If it's a different error, maybe return it or log more severely
		}
	}

	// 2. Remove the container
	removeCtx, removeCancel := context.WithTimeout(ctx, 15*time.Second)
	defer removeCancel()

	m.logger.Debug("Removing container", "containerID", state.ContainerID)
	if err := m.dockerClient.ContainerRemove(removeCtx, state.ContainerID, container.RemoveOptions{Force: true /* Force removal even if running */, RemoveVolumes: true /* Remove associated anonymous volumes */}); err != nil {
		// Log the error, but since we're deleting, maybe it's acceptable if it's already gone
		m.logger.Error("Failed to remove container", "sandboxID", sandboxID, "containerID", state.ContainerID, "error", err)
		// If the container wasn't found, it's effectively deleted from Docker's perspective
		if !client.IsErrNotFound(err) {
			return fmt.Errorf("failed to remove container %s: %w", state.ContainerID, err)
		}
	}

	delete(m.sandboxes, sandboxID)
	// --- End Placeholder --- 

	m.logger.Info("Sandbox deleted successfully", "sandboxID", sandboxID, "containerID", state.ContainerID)
	return nil
}

// ReceiveInternalObservation receives raw observation data from the internal agent (via the handler),
// parses it, broadcasts it through the WebSocket hub, and sends the final 'end' observation
// when a 'result' observation is received.
func (m *SandboxManager) ReceiveInternalObservation(sandboxID string, observationData []byte) error {
	m.mu.RLock()
	_, exists := m.sandboxes[sandboxID]
	m.mu.RUnlock()

	if !exists {
		m.logger.Warn("Received internal observation for non-existent or deleted sandbox", "sandboxID", sandboxID)
		return nil // Don't return error to agent, just ignore
	}

	// Attempt to parse the incoming observation data
	var obs Observation
	if err := json.Unmarshal(observationData, &obs); err != nil {
		m.logger.Error("Failed to unmarshal internal observation from agent", "error", err, "sandboxID", sandboxID, "rawData", string(observationData))
		// Decide if we should broadcast raw data or an error message
		// Broadcasting raw might break clients expecting JSON. Let's send an error observation.
		// We need an actionID here... which we don't have directly. This is a flaw.
		// For now, we can only log the error. We cannot reliably send an error observation without actionID.
		// TODO: Agent MUST include action_id in all pushed observations.
		return fmt.Errorf("failed to parse observation JSON: %w", err) // Return error to agent? Maybe not.
	}

	// Log the received observation
	m.logger.Debug("Received internal observation", "sandboxID", sandboxID, "actionID", obs.ActionID, "type", obs.Type)

	// Ensure actionID is present
	if obs.ActionID == "" {
		m.logger.Error("Received internal observation without action_id", "sandboxID", sandboxID, "type", obs.Type, "rawData", string(observationData))
		// Cannot process further without actionID
		return nil // Ignore observation without actionID
	}

	// Broadcast the received observation via WebSocket hub
	if m.hub != nil {
		// Re-marshal the parsed object to ensure consistent format? Or send raw? Send raw for now.
		m.hub.SubmitBroadcast(sandboxID, observationData)
	}

	// If this is a 'result' observation, also send the final 'end' observation
	if obs.Type == "result" {
		m.logger.Info("Received 'result' observation, sending 'end'", "sandboxID", sandboxID, "actionID", obs.ActionID)

		// Extract exit code and error from the result data
		var exitCode int = -1 // Default if parsing fails
		var errorMsg string
		
		// Attempt to parse the Data field based on expected structure for 'result'
		if dataMap, ok := obs.Data.(map[string]interface{}); ok {
			if ec, ok := dataMap["exit_code"].(float64); ok { // JSON numbers are float64
				exitCode = int(ec)
			} else {
				m.logger.Warn("Could not parse 'exit_code' from result data", "actionID", obs.ActionID, "data", obs.Data)
			}
			if errMsg, ok := dataMap["error"].(string); ok {
				errorMsg = errMsg
			}
		} else {
			m.logger.Warn("Received 'result' observation with unexpected data format", "actionID", obs.ActionID, "data", obs.Data)
			errorMsg = "Result data format unexpected"
		}

		// Send the 'end' observation
		m.pushObservation(sandboxID, obs.ActionID, "end", EndObservationData{ExitCode: exitCode, Error: errorMsg})
	}

	return nil
}