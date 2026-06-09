// Package bench is a retrieval benchmark harness: it ingests a dataset of
// memories and scores each question's gold retrieval (Recall@K, MRR) and
// latency. Runs offline on the committed sample with a deterministic local
// embedder, or against a real endpoint and a converted LongMemEval/LoCoMo set.
package bench

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

//go:embed data/sample.json
var sampleJSON []byte

// Sample returns the committed offline sample dataset.
func Sample() (*Dataset, error) {
	var d Dataset
	if err := json.Unmarshal(sampleJSON, &d); err != nil {
		return nil, fmt.Errorf("bench: parse sample: %w", err)
	}
	return &d, nil
}

// Item is one memory to ingest; Group scopes it to a namespace, empty falls
// back to a shared default.
type Item struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Group   string `json:"group,omitempty"`
}

// Question is a query plus the gold memory IDs it should retrieve. Group must
// match its items; Answer/Category are populated for QA evaluation where available.
type Question struct {
	Query    string   `json:"query"`
	Gold     []string `json:"gold"`
	Group    string   `json:"group,omitempty"`
	Answer   string   `json:"answer,omitempty"`
	Category string   `json:"category,omitempty"`
}

// Dataset is a normalized retrieval benchmark.
type Dataset struct {
	Name      string     `json:"name"`
	Items     []Item     `json:"items"`
	Questions []Question `json:"questions"`
}

// LoadFile reads a dataset in memini's normalized JSON schema.
func LoadFile(path string) (*Dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Dataset
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("bench: parse dataset: %w", err)
	}
	if len(d.Items) == 0 || len(d.Questions) == 0 {
		return nil, fmt.Errorf("bench: dataset %q has no items or questions", path)
	}
	return &d, nil
}

// LoadLongMemEval converts a LongMemEval file: each haystack session becomes an
// item, each question's answer_session_ids becomes its gold set.
func LoadLongMemEval(path string) (*Dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		QuestionID      string          `json:"question_id"`
		QuestionType    string          `json:"question_type"`
		Question        string          `json:"question"`
		Answer          json.RawMessage `json:"answer"`
		AnswerSessionID []string        `json:"answer_session_ids"`
		HaystackIDs     []string        `json:"haystack_session_ids"`
		HaystackData    [][]turn        `json:"haystack_sessions"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("bench: parse longmemeval: %w", err)
	}
	d := &Dataset{Name: "longmemeval"}
	for qi, r := range rows {
		group := r.QuestionID
		if group == "" {
			group = fmt.Sprintf("q-%d", qi)
		}
		for i, sess := range r.HaystackData {
			d.Items = append(d.Items, Item{
				ID: sessionID(r.HaystackIDs, i), Group: group, Content: sessionText(sess),
			})
		}
		d.Questions = append(d.Questions, Question{
			Query: r.Question, Gold: r.AnswerSessionID, Group: group,
			Answer: jsonScalar(r.Answer), Category: r.QuestionType,
		})
	}
	return d, nil
}

// diaIDRe matches LoCoMo dialogue ids like "D1:3", used to parse the (often
// stringified) evidence field robustly.
var diaIDRe = regexp.MustCompile(`D\d+:\d+`)

// sessionKeyRe matches LoCoMo conversation session keys like "session_3".
var sessionKeyRe = regexp.MustCompile(`^session_\d+$`)

// LoadLoCoMo converts the published LoCoMo file into the normalized Dataset.
// Each conversation is its own group/namespace (dialogue ids repeat across
// conversations); each dialogue turn is an item, and each QA's evidence ids are
// its gold set. Questions without evidence (e.g. adversarial) are skipped.
func LoadLoCoMo(path string) (*Dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		SampleID     string                     `json:"sample_id"`
		Conversation map[string]json.RawMessage `json:"conversation"`
		QA           []struct {
			Question string          `json:"question"`
			Answer   json.RawMessage `json:"answer"`
			Evidence json.RawMessage `json:"evidence"`
			Category json.RawMessage `json:"category"`
		} `json:"qa"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("bench: parse locomo: %w", err)
	}

	d := &Dataset{Name: "locomo"}
	for i, r := range rows {
		group := r.SampleID
		if group == "" {
			group = fmt.Sprintf("conv-%d", i)
		}
		for key, rawSess := range r.Conversation {
			if !sessionKeyRe.MatchString(key) {
				continue // skip non-session fields (speaker names, summaries, dates, ...)
			}
			var turns []struct {
				Speaker string `json:"speaker"`
				DiaID   string `json:"dia_id"`
				Text    string `json:"text"`
			}
			if json.Unmarshal(rawSess, &turns) != nil {
				continue
			}
			// Ground each turn with its session date so temporal questions
			// ("when did X happen?") can resolve relative references.
			date := jsonScalar(r.Conversation[key+"_date_time"])
			for _, tn := range turns {
				if tn.DiaID == "" {
					continue
				}
				content := strings.TrimSpace(tn.Speaker + ": " + tn.Text)
				if date != "" {
					content = "[" + date + "] " + content
				}
				d.Items = append(d.Items, Item{ID: tn.DiaID, Group: group, Content: content})
			}
		}
		for _, qa := range r.QA {
			gold := diaIDRe.FindAllString(string(qa.Evidence), -1)
			if len(gold) == 0 {
				continue
			}
			d.Questions = append(d.Questions, Question{
				Query: qa.Question, Gold: gold, Group: group,
				Answer: jsonScalar(qa.Answer), Category: jsonScalar(qa.Category),
			})
		}
	}
	return d, nil
}

type turn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// jsonScalar renders a JSON scalar (string or number) as a plain string.
func jsonScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func sessionID(ids []string, i int) string {
	if i < len(ids) && ids[i] != "" {
		return ids[i]
	}
	return fmt.Sprintf("session-%d", i)
}

func sessionText(turns []turn) string {
	var b strings.Builder
	for _, tn := range turns {
		b.WriteString(tn.Role)
		b.WriteString(": ")
		b.WriteString(tn.Content)
		b.WriteString("\n")
	}
	return b.String()
}
