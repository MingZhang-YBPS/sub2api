package kiro

// 整包超限时的收缩策略。
//
// 旧实现只截短「当前消息正文」和「所有 tool_result 文本」，历史里的用户正文、
// 助手回复正文和图片都动不了。最近若干轮历史又必须保留，于是只要最近几轮里
// 有大块内容，删历史和截文本都压不下来，最终只能报
// "payload exceeds ... without a safe history cut"。
//
// 这里把可截短的范围扩大到历史正文，并在文本不够抵掉超出量时丢弃图片。
// 系统提示对（priming）通过 protect 参数保护，不参与截短。
// 助手 tool_use 的入参依然不动，原因见下方 visitKiroToolUseInputTargets 处的说明。

// kiroShrinkTarget 指向一处可截短的文本。
// tool_use 的入参放在 map[string]any 里，值无法取地址，所以统一用读写闭包表示。
type kiroShrinkTarget struct {
	get func() string
	set func(string)
}

func kiroStringShrinkTarget(p *string) kiroShrinkTarget {
	return kiroShrinkTarget{
		get: func() string { return *p },
		set: func(v string) { *p = v },
	}
}

// walkKiroShrinkTargets 遍历所有可截短文本。
// protect 为前若干条历史的保护数量，用于跳过系统提示对。
func walkKiroShrinkTargets(
	payload *KiroPayload,
	protect int,
	visit func(kiroShrinkTarget),
) {
	if payload == nil {
		return
	}

	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	visit(kiroStringShrinkTarget(&current.Content))
	visitKiroToolResultTargets(current.UserInputMessageContext, visit)

	history := payload.ConversationState.History
	if protect < 0 {
		protect = 0
	}
	for i := protect; i < len(history); i++ {
		if msg := history[i].UserInputMessage; msg != nil {
			visit(kiroStringShrinkTarget(&msg.Content))
			visitKiroToolResultTargets(msg.UserInputMessageContext, visit)
		}
		if msg := history[i].AssistantResponseMessage; msg != nil {
			visit(kiroStringShrinkTarget(&msg.Content))
		}
	}
}

func visitKiroToolResultTargets(
	ctx *KiroUserInputMessageContext,
	visit func(kiroShrinkTarget),
) {
	if ctx == nil {
		return
	}
	for i := range ctx.ToolResults {
		for j := range ctx.ToolResults[i].Content {
			visit(kiroStringShrinkTarget(&ctx.ToolResults[i].Content[j].Text))
		}
	}
}

// visitKiroToolUseInputTargets 故意不存在。
//
// 助手 tool_use 的入参（Write/Edit 的大段文件内容）体积最大，但不能截短：
// 截短等于篡改模型已经执行过的动作记录，后续轮次模型可能把截断后的入参当成
// 自己真实写入的内容，再据此「修复」文件，造成真实文件被truncate。
// kiro_candidate_audit_test.go 的 TestCandidateOversizedToolInputPreserved 和
// kiro_truncation_regression_test.go 的 TestKiroOversizedToolInputLimit
// 明确要求这种情况保持入参原样并显式报错。

func longestKiroShrinkTarget(payload *KiroPayload, protect int) *kiroShrinkTarget {
	var longest *kiroShrinkTarget
	longestLen := 0
	walkKiroShrinkTargets(payload, protect, func(t kiroShrinkTarget) {
		if n := len(t.get()); n > longestLen {
			longestLen = n
			current := t
			longest = &current
		}
	})
	return longest
}

// kiroShrinkableTextBytes 是当前还能截掉的文本总量，用于判断是否必须丢图片。
func kiroShrinkableTextBytes(payload *KiroPayload, protect int) int {
	total := 0
	walkKiroShrinkTargets(payload, protect, func(t kiroShrinkTarget) {
		total += len(t.get())
	})
	return total
}

// shrinkKiroPayloadText 每轮截短当前最长的一处文本，直到进入上限或无可截短内容。
func shrinkKiroPayloadText(payload *KiroPayload, protect int) {
	for {
		size := kiroPayloadByteSize(payload)
		if size <= kiroMaxPayloadBytes {
			return
		}
		target := longestKiroShrinkTarget(payload, protect)
		if target == nil {
			return
		}
		text := target.get()
		if len(text) == 0 {
			return
		}
		nextLen := len(text) - (size - kiroMaxPayloadBytes)
		if nextLen < 0 {
			nextLen = 0
		}
		shortened := truncateUTF8(text, nextLen)
		if len(shortened) >= len(text) {
			target.set("")
			continue
		}
		target.set(shortened)
	}
}

// dropKiroImages 按时间顺序丢弃图片。
// 图片是 base64，截短只会得到坏数据，只能整条丢。
// 当前消息的图片是用户刚提交的，留到最后才丢。
func dropKiroImages(payload *KiroPayload, protect int) {
	if payload == nil {
		return
	}
	history := payload.ConversationState.History
	if protect < 0 {
		protect = 0
	}
	for i := protect; i < len(history); i++ {
		msg := history[i].UserInputMessage
		if msg == nil || len(msg.Images) == 0 {
			continue
		}
		msg.Images = nil
		if kiroPayloadByteSize(payload) <= kiroMaxPayloadBytes {
			return
		}
	}

	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	if len(current.Images) == 0 {
		return
	}
	current.Images = nil
	// 正文为空的纯图片消息在构造阶段会被填成 " "，这里丢图后不能留空正文。
	if current.Content == "" {
		current.Content = " "
	}
}

// fitKiroPayload 尝试把整包压进上限，返回是否成功。
// 先截文本，文本不足以抵掉超出量时先丢图片，避免把所有文本截没了仍然超限。
func fitKiroPayload(payload *KiroPayload, protect int) bool {
	size := kiroPayloadByteSize(payload)
	if size <= kiroMaxPayloadBytes {
		return true
	}

	if kiroShrinkableTextBytes(payload, protect) < size-kiroMaxPayloadBytes {
		dropKiroImages(payload, protect)
	}

	shrinkKiroPayloadText(payload, protect)
	if kiroPayloadByteSize(payload) <= kiroMaxPayloadBytes {
		return true
	}

	dropKiroImages(payload, protect)
	shrinkKiroPayloadText(payload, protect)
	return kiroPayloadByteSize(payload) <= kiroMaxPayloadBytes
}
