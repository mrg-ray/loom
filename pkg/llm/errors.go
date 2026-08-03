// Copyright 2026 Teradata
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package llm

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// ErrContextTooLong marks a provider refusal positively identified as
// "context too long" (HLD §5.2 step 12). It is the ONLY relief trigger:
// identification is positive, per provider — anthropic: status 400,
// error.type="invalid_request_error", message containing "prompt is too long";
// OpenAI-shaped (LiteLLM): status 400, error.code="context_length_exceeded".
// An error not positively identified is NOT context-too-long and propagates as
// today.
var ErrContextTooLong = errors.New("context too long")

// IsAnthropicContextTooLong positively identifies anthropic's prompt-too-long
// refusal from the HTTP status and raw response body.
func IsAnthropicContextTooLong(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	var resp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false
	}
	return resp.Error.Type == "invalid_request_error" &&
		strings.Contains(resp.Error.Message, "prompt is too long")
}

// IsOpenAIContextTooLong positively identifies the OpenAI-shaped (LiteLLM)
// context-length refusal from the HTTP status and raw response body.
func IsOpenAIContextTooLong(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	var resp struct {
		Error struct {
			Code interface{} `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false
	}
	code, _ := resp.Error.Code.(string)
	return code == "context_length_exceeded"
}
