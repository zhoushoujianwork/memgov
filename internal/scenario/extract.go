package scenario

import "strings"

// Extract recognizes a small explicit Chinese status grammar. It is not a
// general semantic/LLM summarizer. Unknown statements remain unclassified.
// Evidence is reset when status changes and accumulated for corroboration.
func Extract(messages []Message) []Finding {
	var out []Finding
	update := func(topic, status, id string) {
		topic = strings.TrimSpace(topic)
		if topic == "" || len([]rune(topic)) > 24 {
			return
		}
		for i := range out {
			if out[i].Topic == topic {
				if out[i].Status != status && !(out[i].Status == "ready" && status == "in_progress") {
					out[i].Evidence = nil
				}
				out[i].Status = status
				for _, existing := range out[i].Evidence {
					if existing == id {
						return
					}
				}
				out[i].Evidence = append(out[i].Evidence, id)
				return
			}
		}
		out = append(out, Finding{Topic: topic, Status: status, Evidence: []string{id}})
	}
	resolve := func(topic string) string {
		match := ""
		for _, f := range out {
			if strings.HasSuffix(f.Topic, topic) {
				if match != "" {
					return topic
				}
				match = f.Topic
			}
		}
		if match != "" {
			return match
		}
		return topic
	}
	for _, m := range messages {
		// Split into clauses. Do not execute, route commands from, or infer tasks
		// from imperative message content.
		clauses := strings.FieldsFunc(m.Text, func(r rune) bool { return strings.ContainsRune("，。；！？\n", r) })
		for _, clause := range clauses {
			clause = strings.TrimSpace(clause)
			if before, _, ok := strings.Cut(clause, "被"); ok && strings.HasSuffix(clause, "阻塞") {
				update(before, "blocked", m.ID)
				continue
			}
			matched := false
			for _, rule := range []struct{ phrase, status string }{{"已恢复进行", "in_progress"}, {"正在进行", "in_progress"}, {"已完成", "done"}, {"还没定", "pending_confirmation"}} {
				if strings.HasSuffix(clause, rule.phrase) {
					topic := strings.TrimSuffix(clause, rule.phrase)
					update(resolve(topic), rule.status, m.ID)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			// A request to continue alone does not prove resumed work. Require an
			// explicit cleared dependency in the same message and an existing topic.
			if strings.HasPrefix(clause, "请继续") && strings.Contains(m.Text, "已开通") {
				topic := resolve(strings.TrimPrefix(clause, "请继续"))
				for _, f := range out {
					if f.Topic == topic {
						update(topic, "ready", m.ID)
						break
					}
				}
			}
		}
	}
	return out
}
