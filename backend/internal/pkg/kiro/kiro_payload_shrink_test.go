package kiro

import (
	"strings"
	"testing"
)

// 构造一段无法删减的历史：条数不超过 kiroMinRecentHistoryTurns，
// 所以超限只能靠截短正文或丢图片解决，无法靠丢历史解决。
func makeUndroppableHistory(
	bigUser string,
	bigAssistant string,
	images []KiroImage,
) *KiroPayload {
	return &KiroPayload{
		ConversationState: KiroConversationState{
			ChatTriggerType: "MANUAL",
			ConversationID:  "test",
			CurrentMessage: KiroCurrentMessage{
				UserInputMessage: KiroUserInputMessage{
					Content: "continue",
					ModelID: "claude-sonnet-4.5",
					Origin:  "AI_EDITOR",
				},
			},
			History: []KiroHistoryMessage{
				{UserInputMessage: &KiroUserInputMessage{
					Content: "start",
					Images:  images,
				}},
				{AssistantResponseMessage: &KiroAssistantResponseMessage{
					Content: bigAssistant,
				}},
				{UserInputMessage: &KiroUserInputMessage{
					Content: bigUser,
				}},
				{AssistantResponseMessage: &KiroAssistantResponseMessage{
					Content: "done",
				}},
			},
		},
	}
}

// 历史里助手回复过大时，应截短后放行，而不是报 without a safe history cut。
func TestShrinkTruncatesHistoryAssistantContent(t *testing.T) {
	big := strings.Repeat("A", kiroMaxPayloadBytes+64*1024)
	p := makeUndroppableHistory("ok", big, nil)

	if err := truncateKiroPayloadToLimit(p, false); err != nil {
		t.Fatalf("expected payload to fit, got %v", err)
	}
	if size := kiroPayloadByteSize(p); size > kiroMaxPayloadBytes {
		t.Fatalf("payload still over limit: %d", size)
	}
	if len(p.ConversationState.History) != 4 {
		t.Fatalf("recent history was discarded: %d entries",
			len(p.ConversationState.History))
	}
	got := p.ConversationState.History[1].AssistantResponseMessage.Content
	if len(got) >= len(big) {
		t.Fatal("assistant content was not shortened")
	}
}

// 历史里用户正文过大时同样应截短后放行。
func TestShrinkTruncatesHistoryUserContent(t *testing.T) {
	big := strings.Repeat("B", kiroMaxPayloadBytes+64*1024)
	p := makeUndroppableHistory(big, "fine", nil)

	if err := truncateKiroPayloadToLimit(p, false); err != nil {
		t.Fatalf("expected payload to fit, got %v", err)
	}
	if size := kiroPayloadByteSize(p); size > kiroMaxPayloadBytes {
		t.Fatalf("payload still over limit: %d", size)
	}
	got := p.ConversationState.History[2].UserInputMessage.Content
	if len(got) >= len(big) {
		t.Fatal("user content was not shortened")
	}
}

// 图片是 base64，截短只会得到坏数据，超限时应整条丢弃而不是报错。
func TestShrinkDropsOversizedHistoryImages(t *testing.T) {
	images := []KiroImage{{
		Format: "png",
		Source: KiroImageSource{
			Bytes: strings.Repeat("C", kiroMaxPayloadBytes+64*1024),
		},
	}}
	p := makeUndroppableHistory("ok", "fine", images)

	if err := truncateKiroPayloadToLimit(p, false); err != nil {
		t.Fatalf("expected payload to fit, got %v", err)
	}
	if size := kiroPayloadByteSize(p); size > kiroMaxPayloadBytes {
		t.Fatalf("payload still over limit: %d", size)
	}
	if got := p.ConversationState.History[0].UserInputMessage.Images; len(got) != 0 {
		t.Fatal("oversized history image was not dropped")
	}
	// 图片被丢掉后，正文不应被连带截短。
	if p.ConversationState.History[2].UserInputMessage.Content != "ok" {
		t.Fatal("text was shortened even though dropping the image sufficed")
	}
}

// 系统提示对必须完整保留，不能被当成可截短文本。
func TestShrinkPreservesPrimingPair(t *testing.T) {
	const systemPrompt = "SYSTEM PROMPT MUST SURVIVE"
	big := strings.Repeat("D", kiroMaxPayloadBytes+64*1024)

	p := &KiroPayload{
		ConversationState: KiroConversationState{
			ChatTriggerType: "MANUAL",
			ConversationID:  "test",
			CurrentMessage: KiroCurrentMessage{
				UserInputMessage: KiroUserInputMessage{
					Content: "continue",
					ModelID: "claude-sonnet-4.5",
					Origin:  "AI_EDITOR",
				},
			},
			History: []KiroHistoryMessage{
				{UserInputMessage: &KiroUserInputMessage{Content: systemPrompt}},
				{AssistantResponseMessage: &KiroAssistantResponseMessage{
					Content: "understood",
				}},
				{UserInputMessage: &KiroUserInputMessage{Content: "start"}},
				{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: big}},
				{UserInputMessage: &KiroUserInputMessage{Content: "ok"}},
				{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "done"}},
			},
		},
	}

	if err := truncateKiroPayloadToLimit(p, true); err != nil {
		t.Fatalf("expected payload to fit, got %v", err)
	}
	if size := kiroPayloadByteSize(p); size > kiroMaxPayloadBytes {
		t.Fatalf("payload still over limit: %d", size)
	}
	first := p.ConversationState.History[0].UserInputMessage
	if first == nil || first.Content != systemPrompt {
		t.Fatalf("priming user message was modified: %+v", first)
	}
	second := p.ConversationState.History[1].AssistantResponseMessage
	if second == nil || second.Content != "understood" {
		t.Fatalf("priming assistant message was modified: %+v", second)
	}
}
