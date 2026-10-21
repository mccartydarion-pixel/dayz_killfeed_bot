package discord

// designPreviewReplies is how typical command answers read: one success, one refusal, and the
// ways something can be unavailable or fail. Every sentence comes from voice.go or is copied
// from the handler that says it.
func designPreviewReplies() []designPreviewCard {
	reply := func(id, title, content string) designPreviewCard {
		return designPreviewCard{ID: id, Title: title, Group: "Command replies", Source: designPreviewSource, Content: content}
	}
	return []designPreviewCard{
		reply("reply-success", "Command answer: it worked", "✅ Verified role set. It will be assigned automatically when a /link request is verified."),
		reply("reply-refusal", "Command answer: not allowed", ReplyNeedsPermission("run `/setup`")),
		reply("reply-not-linked", "Command answer: account not linked", ReplyNotLinked("Link your PlayStation username first in #link-username.")),
		reply("reply-unavailable", "Command answer: feature unavailable", ReplyAreUnavailable("Stats")),
		reply("reply-error", "Command answer: something failed", ReplyCouldNot("load stats")),
		reply("reply-crash", "Command answer: unexpected failure", interactionFailureText),
	}
}
