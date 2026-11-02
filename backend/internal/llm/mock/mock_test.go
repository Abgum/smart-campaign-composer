package mock_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/mock"
)

func run(t *testing.T, c llm.Client, req llm.Request) ([]llm.Event, error) {
	t.Helper()
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	var out []llm.Event
	for s.Next() {
		out = append(out, s.Event())
	}
	return out, s.Err()
}

func userReq(text string) llm.Request {
	return llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}}}
}

func TestMockScenarioPlayback(t *testing.T) {
	input := `{"name":"Sepet hatırlatma","sms":{"text":"Merhaba {{first_name}}, sepetin seni bekliyor!"}}`
	m := mock.NewScripted(
		mock.Call{Text: "Taslağı hazırlıyorum.", ToolName: "save_campaign_draft", ToolInput: input},
		mock.Call{Text: "Kaydettim, 312 kişiye ulaşıyor."},
	)

	events, err := run(t, m, userReq("İstanbul sepet hatırlatma"))
	if err != nil {
		t.Fatal(err)
	}
	var text, rawInput strings.Builder
	var end llm.MessageEnd
	var toolEnd llm.ToolUseEnd
	deltas := 0
	for _, e := range events {
		switch e := e.(type) {
		case llm.TextDelta:
			text.WriteString(e.Text)
		case llm.ToolInputDelta:
			deltas++
			rawInput.WriteString(e.PartialJSON)
		case llm.ToolUseEnd:
			toolEnd = e
		case llm.MessageEnd:
			end = e
		}
	}
	if text.String() != "Taslağı hazırlıyorum." {
		t.Errorf("text = %q", text.String())
	}
	if deltas < 2 {
		t.Errorf("tool input must stream in several chunks, got %d", deltas)
	}
	if rawInput.String() != input || string(toolEnd.RawInput) != input || toolEnd.Name != "save_campaign_draft" || toolEnd.ID == "" {
		t.Errorf("tool input = %q / end %+v", rawInput.String(), toolEnd)
	}
	if end.StopReason != llm.StopToolUse || len(end.Content) != 2 ||
		end.Content[0].Type != llm.BlockText || end.Content[1].Type != llm.BlockToolUse || end.Content[1].ToolUseID != toolEnd.ID {
		t.Errorf("message end = %+v", end)
	}

	events, err = run(t, m, userReq("devam"))
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].(llm.MessageEnd).StopReason; got != llm.StopEndTurn {
		t.Errorf("second call stop = %q", got)
	}
	if m.Calls() != 2 || len(m.Requests()) != 2 || m.Requests()[0].Messages[0].Blocks[0].Text != "İstanbul sepet hatırlatma" {
		t.Errorf("calls=%d requests=%+v", m.Calls(), m.Requests())
	}
	if _, err := run(t, m, userReq("fazla")); err == nil {
		t.Error("a call beyond the script must fail")
	}
}

func TestMock_Errors(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{Err: llm.ErrUnavailable},
		mock.Call{Text: "Yarım kalan yanıt", Err: llm.ErrStreamInterrupted, ErrAfterEvents: 2},
	)
	events, err := run(t, m, userReq("a"))
	if !errors.Is(err, llm.ErrUnavailable) || len(events) != 0 {
		t.Fatalf("err=%v events=%d", err, len(events))
	}
	events, err = run(t, m, userReq("b"))
	if !errors.Is(err, llm.ErrStreamInterrupted) {
		t.Fatalf("err = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events before the error, got %d", len(events))
	}
	for _, e := range events {
		if _, ok := e.(llm.MessageEnd); ok {
			t.Fatal("interrupted stream must not end the message")
		}
	}
}

func TestMock_StopReasonsAndRawInput(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: `{"name": "Sepet hat`, Stop: llm.StopMaxTokens},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: `{"name": "x",}`},
		mock.Call{Text: "Bu isteğe yardımcı olamam.", Stop: llm.StopRefusal},
	)
	events, _ := run(t, m, userReq("1"))
	if end := events[len(events)-1].(llm.MessageEnd); end.StopReason != llm.StopMaxTokens {
		t.Errorf("stop = %q", end.StopReason)
	}
	events, _ = run(t, m, userReq("2"))
	for _, e := range events {
		if te, ok := e.(llm.ToolUseEnd); ok && string(te.RawInput) != `{"name": "x",}` {
			t.Errorf("invalid input must pass through unchanged, got %s", te.RawInput)
		}
	}
	events, _ = run(t, m, userReq("3"))
	if end := events[len(events)-1].(llm.MessageEnd); end.StopReason != llm.StopRefusal {
		t.Errorf("stop = %q", end.StopReason)
	}
}

func TestMock_Info(t *testing.T) {
	if info := mock.NewScripted().Info(); info.Provider != "mock" {
		t.Fatalf("info = %+v", info)
	}
}
