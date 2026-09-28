package agentsearch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ChatJSONStream emits only the public summary, never raw JSON or reasoning tokens.
func (c *LLMClient) ChatJSONStream(ctx context.Context, system, user string, onSummary func(string) error) (string, error) {
	res, err := c.openChat(ctx, system, user, true)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		return "", fmt.Errorf("模型网关未返回流式响应")
	}
	scanner := bufio.NewScanner(io.LimitReader(res.Body, 4<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var content strings.Builder
	var data []string
	lastSummary, summaryDone := "", false
	consume := func(payload string) (bool, error) {
		if payload == "[DONE]" {
			return true, nil
		}
		var chunk struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return false, fmt.Errorf("模型流格式错误")
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return false, fmt.Errorf("模型流返回错误")
		}
		if len(chunk.Choices) == 0 {
			return false, nil
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" && choice.FinishReason != "stop" {
			return false, fmt.Errorf("模型流未完整结束")
		}
		content.WriteString(choice.Delta.Content)
		if !summaryDone && choice.Delta.Content != "" {
			var summary string
			summary, summaryDone = partialSummary(content.String())
			summary = truncateRunes(strings.TrimSpace(summary), 120)
			if summary != "" && summary != lastSummary {
				if err := onSummary(summary); err != nil {
					return false, err
				}
				lastSummary = summary
			}
		}
		return false, nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		} else if line == "" && len(data) > 0 {
			done, err := consume(strings.Join(data, "\n"))
			data = nil
			if err != nil {
				return "", err
			}
			if done {
				if content.Len() == 0 {
					return "", fmt.Errorf("模型未返回内容")
				}
				return content.String(), nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", io.ErrUnexpectedEOF
}

// A decoder walks root fields so nested/user-quoted "summary" keys cannot leak.
func partialSummary(content string) (string, bool) {
	raw := stripCodeFence(content)
	decoder := json.NewDecoder(strings.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return "", false
	}
	relevant, savedSummary := false, ""
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", false
		}
		if key == "summary" && relevant {
			tail := strings.TrimLeft(raw[decoder.InputOffset():], " \n\r\t")
			if !strings.HasPrefix(tail, ":") {
				return "", false
			}
			return partialJSONString(strings.TrimLeft(tail[1:], " \n\r\t"))
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return "", false
		}
		switch key {
		case "relevant":
			if json.Unmarshal(value, &relevant) != nil {
				return "", false
			}
		case "summary":
			if json.Unmarshal(value, &savedSummary) != nil {
				return "", false
			}
		}
		if relevant && savedSummary != "" {
			return savedSummary, true
		}
	}
	return "", false
}

// Only decode complete rune/escape boundaries, including UTF-16 surrogate pairs.
func partialJSONString(raw string) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	end, complete := 1, false
	for end < len(raw) {
		if raw[end] == '"' {
			complete = true
			break
		}
		if raw[end] == '\\' {
			if end+1 >= len(raw) {
				break
			}
			if raw[end+1] != 'u' {
				if !strings.ContainsRune(`"\/bfnrt`, rune(raw[end+1])) {
					break
				}
				end += 2
				continue
			}
			if end+6 > len(raw) {
				break
			}
			value, err := strconv.ParseUint(raw[end+2:end+6], 16, 16)
			if err != nil || value >= 0xDC00 && value <= 0xDFFF {
				break
			}
			if value >= 0xD800 && value <= 0xDBFF {
				if end+12 > len(raw) || raw[end+6:end+8] != `\u` {
					break
				}
				low, err := strconv.ParseUint(raw[end+8:end+12], 16, 16)
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					break
				}
				end += 12
			} else {
				end += 6
			}
		} else {
			r, size := utf8.DecodeRuneInString(raw[end:])
			if r < ' ' || r == utf8.RuneError && size == 1 {
				break
			}
			end += size
		}
	}
	var value string
	if json.Unmarshal([]byte(raw[:end]+`"`), &value) != nil {
		return "", false
	}
	return value, complete
}
