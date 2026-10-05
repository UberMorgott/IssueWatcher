// Package replystyle keeps drafted replies short and human (the owner's rule
// for every reply an agent drafts: manual drafts, autopilot replies, triage
// replies). It holds the style section every reply prompt gets, the
// post-check that refuses AI-slop drafts (one redraft, then a template) and
// the minimal templates a refused draft falls back to.
package replystyle

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Reply kinds: what the item is (the fix agent's non-bug outcome, the
// triage verdict) or what the reply says (autopilot draft kinds).
const (
	KindFeedback   = "feedback"   // thanks / praise / a general comment: nothing broken, nothing asked
	KindQuestion   = "question"   // the reporter asks something
	KindSuggestion = "suggestion" // an idea or feature request
	KindNeedsInfo  = "needs_info" // a bug report that lacks details
	KindReleased   = "released"   // the fix is out
)

// NonBug reports whether kind is one of the non-bug kinds a fix run may end with.
func NonBug(kind string) bool {
	return kind == KindFeedback || kind == KindQuestion || kind == KindSuggestion
}

// Feedback replies: 1–3 short sentences.
const (
	maxFeedbackSentences = 3
	maxFeedbackRunes     = 280
	echoWords            = 6 // a run of this many of the reporter's words repeated back
)

// banned are filler / hype phrases (lower case, straight apostrophes) a
// draft must not contain.
var banned = []string{
	// English
	"i'm thrilled", "i am thrilled", "so thrilled", "means a lot", "means the world", "kind words",
	"happy gaming", "happy modding", "don't hesitate", "do not hesitate", "feel free", "reach out",
	"great question", "hope this helps", "rest assured", "delighted", "it's wonderful", "truly appreciate",
	"thrilled to hear", "glad to hear that you're enjoying", "if you have any other questions",
	"if you have any more questions", "if you have any further questions", "best regards", "kind regards",
	"warm regards", "as an ai",
	// Russian
	"не стесняйтесь", "не стесняйся", "обращайтесь", "приятной игры", "тёплые слова", "теплые слова",
	"очень ценю", "много значит", "буду рад помочь", "буду рада помочь", "если у вас возникнут", "если возникнут вопросы",
	"с уважением",
}

// Rules is the style section of a reply prompt for kind ("" = any reply).
func Rules(kind string) string {
	s := "\n\nReply style (the app checks it and refuses a draft that breaks it):\n" +
		"- Short and normal, like a person typing a quick comment, not a support bot.\n" +
		"- In the reporter's language, with plain everyday words.\n" +
		"- No hype or filler: never phrases like \"I'm thrilled\", \"means a lot\", \"Thank you so much for your kind words\", " +
		"\"Happy gaming!\", \"Don't hesitate to reach out\", \"feel free to\", \"I hope this helps\", \"Great question\".\n" +
		"- No emoji unless the reporter used them. No em-dash chains (at most one dash in the whole reply).\n" +
		"- Do not repeat or quote the reporter's message back to them.\n" +
		"- No greeting line, no sign-off, no signature."
	switch kind {
	case KindFeedback:
		s += "\nThis is thanks or praise, not a bug report: answer in 1–3 short sentences, for example \"Thanks, glad it works for you!\". " +
			"Nothing else: no feature pitch, no questions back, no list of what the mod does."
	case KindSuggestion:
		s += "\nThis is a suggestion or idea, not a bug: thank them in a few words and say plainly whether it fits or that you will think about it. " +
			"1–3 short sentences, no promises, no dates."
	case KindQuestion:
		s += "\nThis is a question, not a bug: answer it directly in a few short sentences."
	}
	return s
}

// Input is what Check judges a draft against.
type Input struct {
	Kind string
	// Source is the reporter's own text (title, body, their comments); "" skips
	// the checks that need it (emoji the reporter used, echo of their words).
	// (Echo is judged on replies to thanks and ideas only: a bug answer may name the problem.)
	Source string
	Limit  int // max characters (0 = none)
}

// Check returns the style problems of draft text (none = it passes).
func Check(text string, in Input) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{"the reply is empty"}
	}
	var out []string
	n := len([]rune(text))
	if in.Limit > 0 && n > in.Limit {
		out = append(out, "it is "+strconv.Itoa(n)+" characters, the limit is "+strconv.Itoa(in.Limit))
	}
	low := normalize(text)
	for _, p := range banned {
		if strings.Contains(low, p) {
			out = append(out, "it uses the filler phrase \""+p+"\"")
		}
	}
	if hasEmoji(text) && (in.Source == "" || !hasEmoji(in.Source)) {
		out = append(out, "it uses emoji the reporter did not use")
	}
	if strings.Count(text, "—")+strings.Count(text, " – ")+strings.Count(text, " -- ") >= 2 {
		out = append(out, "it chains em-dashes")
	}
	if signature(text) {
		out = append(out, "it ends with a sign-off or signature")
	}
	if in.Source != "" && (in.Kind == KindFeedback || in.Kind == KindSuggestion) && echoes(text, in.Source) {
		out = append(out, "it repeats the reporter's message back")
	}
	if in.Kind == KindFeedback {
		if s := sentences(text); s > maxFeedbackSentences {
			out = append(out, "a reply to thanks must be 1–3 short sentences, it has "+strconv.Itoa(s))
		}
		if n > maxFeedbackRunes {
			out = append(out, "a reply to thanks must be short (at most "+strconv.Itoa(maxFeedbackRunes)+" characters)")
		}
	}
	return out
}

// RedraftNote asks the agent to write the reply again without problems.
func RedraftNote(problems []string) string {
	return "\n\nYour previous draft was refused: " + strings.Join(problems, "; ") +
		". Write it again, shorter and plainer, following the reply style above."
}

// Fallback is the minimal template reply of kind in language lang (BCP 47;
// Russian or English).
func Fallback(kind, lang string) string {
	ru := strings.HasPrefix(strings.ToLower(lang), "ru")
	pick := func(en, r string) string {
		if ru {
			return r
		}
		return en
	}
	switch kind {
	case KindFeedback:
		return pick("Thanks, glad it works for you!", "Спасибо, рад, что всё работает!")
	case KindSuggestion:
		return pick("Thanks for the idea, I'll think about it.", "Спасибо за идею, подумаю.")
	case KindQuestion:
		return pick("Thanks, I'll look into it and get back to you.", "Спасибо, посмотрю и отвечу.")
	case KindNeedsInfo:
		return pick("Thanks for the report! Could you add the steps to reproduce it, the mod and game versions, and the error or log if there is one?",
			"Спасибо за репорт! Напишите, пожалуйста, шаги воспроизведения, версии мода и игры и текст ошибки или лог, если есть.")
	}
	return pick("Thanks for the report, I'll take a look.", "Спасибо, посмотрю.")
}

// Language guesses the language of text: "ru" when it is mostly Cyrillic, else "en".
func Language(text string) string {
	cyr, lat := 0, 0
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyr++
		case unicode.Is(unicode.Latin, r):
			lat++
		}
	}
	if cyr > lat {
		return "ru"
	}
	return "en"
}

func normalize(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer("’", "'", "‘", "'", "ʼ", "'").Replace(s)
}

func hasEmoji(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x1F000 && r <= 0x1FAFF, r >= 0x2600 && r <= 0x27BF, r == 0x2B50, r == 0x2B55, r == 0xFE0F, r == 0x2764:
			return true
		}
	}
	return false
}

// signatureRe: a last line that is only a sign-off or a dash and a name.
var signatureRe = regexp.MustCompile(`(?i)^(?:[-—–]{1,2}\s*[\p{L}][\p{L}\p{N}_. ]{0,30}|(?:best|kind|warm)?\s*regards,?|cheers[,!]?|best,|sincerely,?|с уважением,?|всего доброго[,!]?)$`)

func signature(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) < 2 {
		return false
	}
	return signatureRe.MatchString(strings.TrimSpace(lines[len(lines)-1]))
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}']+`)

func words(s string) []string { return wordRe.FindAllString(normalize(s), -1) }

// echoes: the draft repeats a run of echoWords consecutive words of the source.
func echoes(text, source string) bool {
	src := words(source)
	if len(src) < echoWords {
		return false
	}
	seen := map[string]bool{}
	for i := 0; i+echoWords <= len(src); i++ {
		seen[strings.Join(src[i:i+echoWords], " ")] = true
	}
	dw := words(text)
	for i := 0; i+echoWords <= len(dw); i++ {
		if seen[strings.Join(dw[i:i+echoWords], " ")] {
			return true
		}
	}
	return false
}

var sentenceEnd = regexp.MustCompile(`[.!?…]+(?:\s|$)`)

func sentences(text string) int {
	n := 0
	for _, p := range sentenceEnd.Split(strings.TrimSpace(text), -1) {
		if strings.TrimSpace(p) != "" {
			n++
		}
	}
	return n
}
