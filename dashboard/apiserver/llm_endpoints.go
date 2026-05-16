// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import (
	"encoding/json"
	"net/http"

	"github.com/sirus20x6/adamaton-core/types"
)

// getLLMStatus returns the status of the LLM backend
func (s *APIServer) getLLMStatus(w http.ResponseWriter, r *http.Request) {
	err := s.vllmClient.Health(r.Context())

	status := map[string]interface{}{
		"backend":  s.config.LLM.Backend,
		"endpoint": s.config.LLM.Endpoint,
		"healthy":  err == nil,
		"model":    s.config.LLM.ModelName,
		"chat_api": s.config.LLM.UseChatAPI,
	}

	if err != nil {
		status["error"] = err.Error()
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    status,
		Success: err == nil,
	})
}

// testLLMGeneration tests the LLM backend with a simple prompt
func (s *APIServer) testLLMGeneration(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Prompt      string  `json:"prompt"`
		MaxTokens   int     `json:"max_tokens,omitempty"`
		Temperature float64 `json:"temperature,omitempty"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error:   "Invalid request format",
			Success: false,
		})
		return
	}

	if request.Prompt == "" {
		request.Prompt = "Please respond with 'Hello, I am working correctly!' to test the connection."
	}

	agentConfig := types.AgentConfig{
		MaxTokens:   request.MaxTokens,
		Temperature: request.Temperature,
	}

	if agentConfig.MaxTokens == 0 {
		agentConfig.MaxTokens = 100
	}
	if agentConfig.Temperature == 0 {
		agentConfig.Temperature = 0.7
	}

	result, err := s.vllmClient.ExecuteAgentAnalysis(r.Context(), types.AgentSecurity, request.Prompt, agentConfig)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error:   err.Error(),
			Success: false,
		})
		return
	}

	response := map[string]interface{}{
		"prompt":      request.Prompt,
		"response":    result.Rationale,
		"backend":     s.config.LLM.Backend,
		"model":       s.config.LLM.ModelName,
		"max_tokens":  agentConfig.MaxTokens,
		"temperature": agentConfig.Temperature,
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    response,
		Success: true,
	})
}

// getVLLMCompatibilityInfo returns information about vLLM endpoint compatibility
func (s *APIServer) getVLLMCompatibilityInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]interface{}{
		"vllm_endpoint":     s.config.LLM.Endpoint,
		"openai_compatible": true,
		"supported_endpoints": []string{
			"/v1/completions",
			"/v1/chat/completions",
			"/v1/models",
			"/health",
		},
		"recommended_settings": map[string]interface{}{
			"use_chat_api": true,
			"max_tokens_per_agent": map[string]int{
				"security":       512,
				"performance":    512,
				"architecture":   768,
				"business_logic": 768,
				"default":        512,
			},
			"temperature_by_agent": map[string]float64{
				"security":      0.1,
				"performance":   0.1,
				"architecture":  0.2,
				"documentation": 0.2,
				"default":       0.1,
			},
		},
		"vllm_optimization": map[string]interface{}{
			"tensor_parallel_size":    2,
			"max_num_batched_tokens": 16384,
			"max_num_seqs":           64,
			"gpu_memory_utilization": 0.95,
		},
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    info,
		Success: true,
	})
}