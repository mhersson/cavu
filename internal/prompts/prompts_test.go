package prompts_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mhersson/cavu/internal/prompts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEveryPromptRenders(t *testing.T) {
	findings := []prompts.ReviewFinding{
		{Number: 31, Severity: "critical", Where: "where-a", What: "what-b", Why: "why-c"},
		{Number: 32, Severity: "minor", Where: "where-d", What: "what-e", Why: "why-f"},
	}
	found := []string{"31", "where-a", "what-b", "why-c", "32", "where-d", "what-e", "why-f"}
	// A finding without its fields, in data that has every other field.
	partial := struct {
		Title, Body, DiffFile, FixDiffFile string
		Findings                           []struct{ Number int }
	}{Findings: []struct{ Number int }{{Number: 1}}}

	tests := []struct {
		name   string
		data   any
		values []string // what the data must put into the prompt
		wrong  []any    // data that must fail besides the ones every prompt with data refuses
	}{
		{name: prompts.Instructions},
		{name: prompts.Work, data: prompts.WorkData{Title: "title-1", Body: "body-2"}, values: []string{"title-1", "body-2"}},
		{name: prompts.Continue},
		{
			name: prompts.FixVerify, data: prompts.FixVerifyData{Command: "command-3", Output: "output-4"},
			values: []string{"command-3", "output-4"},
		},
		{name: prompts.Answer, data: prompts.AnswerData{Answer: "answer-5"}, values: []string{"answer-5"}},
		{name: prompts.Doctor},
		{name: prompts.Readiness, data: prompts.WorkData{Title: "title-6", Body: "body-7"}, values: []string{"title-6", "body-7"}},
		{name: prompts.Proceed},
		{
			name: prompts.Review,
			data: prompts.ReviewData{
				Title: "title-8", Body: "body-9", Answers: []string{"answer-10"}, Assumptions: []string{"assumption-11"},
				Summary: "summary-12", DiffFile: "/diff-13",
			},
			values: []string{"title-8", "body-9", "answer-10", "assumption-11", "summary-12", "/diff-13"},
		},
		{
			name:   prompts.Rereview,
			data:   prompts.RereviewData{Title: "title-14", Body: "body-15", Findings: findings, DiffFile: "/diff-16", FixDiffFile: "/fix-17"},
			values: append([]string{"title-14", "body-15", "/diff-16", "/fix-17"}, found...), wrong: []any{partial},
		},
		{name: prompts.FixReview, data: prompts.FixReviewData{Findings: findings}, values: found, wrong: []any{partial}},
		{
			name:   prompts.FixReviewFresh,
			data:   prompts.FixReviewFreshData{Title: "title-18", Body: "body-19", Findings: findings, DiffFile: "/diff-20"},
			values: append([]string{"title-18", "body-19", "/diff-20"}, found...), wrong: []any{partial},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := prompts.Render(tt.name, tt.data)
			require.NoError(t, err)
			assert.NotEmpty(t, strings.TrimSpace(got))
			assert.NotContains(t, got, "{{")
			assert.NotContains(t, got, "<no value>")

			for _, v := range tt.values {
				assert.Contains(t, got, v)
			}

			if tt.data == nil {
				return
			}

			// A placeholder the data does not fill is an error, never an empty string.
			for _, wrong := range append([]any{nil, struct{ Other string }{}, map[string]string{}}, tt.wrong...) {
				_, err := prompts.Render(tt.name, wrong)
				assert.Error(t, err, "%#v", wrong)
			}
		})
	}

	t.Run("an unknown name", func(t *testing.T) {
		_, err := prompts.Render("result.schema", nil)
		require.Error(t, err)
	})
}

func TestResultSchemaIsValidJSON(t *testing.T) {
	raw := prompts.ResultSchema()
	require.True(t, json.Valid(raw))

	type array struct {
		Type  string
		Items struct{ Type string }
	}

	var schema struct {
		Type       string
		Required   []string
		Properties struct {
			Status                 struct{ Enum []string }
			Title, Summary         struct{ Type string }
			Assumptions, Questions array
		}
		If struct {
			Properties struct{ Status struct{ Const string } }
		}
		Then struct {
			Required   []string
			Properties struct {
				Questions struct {
					Type     string
					MinItems int
				}
			}
		}
	}

	require.NoError(t, json.Unmarshal(raw, &schema))
	assert.Equal(t, "object", schema.Type)
	assert.Equal(t, []string{"status"}, schema.Required)
	assert.ElementsMatch(t, []string{"done", "blocked", "failed"}, schema.Properties.Status.Enum)
	assert.Equal(t, "string", schema.Properties.Title.Type)
	assert.Equal(t, "string", schema.Properties.Summary.Type)

	for _, a := range []array{schema.Properties.Assumptions, schema.Properties.Questions} {
		assert.Equal(t, "array", a.Type)
		assert.Equal(t, "string", a.Items.Type)
	}

	assert.Equal(t, "blocked", schema.If.Properties.Status.Const, "the condition is a blocked status")
	assert.Equal(t, []string{"questions"}, schema.Then.Required)
	assert.Equal(t, "array", schema.Then.Properties.Questions.Type)
	assert.Equal(t, 1, schema.Then.Properties.Questions.MinItems)

	raw[0] = 'x'

	assert.True(t, json.Valid(prompts.ResultSchema()), "every call returns its own copy")
}

func TestReadinessSchemaIsValidJSON(t *testing.T) {
	raw := prompts.ReadinessSchema()
	require.True(t, json.Valid(raw))

	var schema struct {
		Required   []string
		Properties struct {
			Status    struct{ Enum []string }
			Questions struct {
				Type  string
				Items struct {
					Type       string
					Required   []string
					Properties map[string]struct{ Type string }
				}
			}
		}
		If struct {
			Properties struct{ Status struct{ Const string } }
		}
		Then struct {
			Required   []string
			Properties struct {
				Questions struct{ MinItems int }
			}
		}
	}

	require.NoError(t, json.Unmarshal(raw, &schema))
	assert.Equal(t, []string{"status"}, schema.Required)
	assert.ElementsMatch(t, []string{"ready", "blocked"}, schema.Properties.Status.Enum)

	items := schema.Properties.Questions.Items
	assert.Equal(t, "array", schema.Properties.Questions.Type)
	assert.Equal(t, "object", items.Type, "each question is a pair")
	assert.ElementsMatch(t, []string{"question", "suggested_answer"}, items.Required)

	for _, k := range []string{"question", "suggested_answer"} {
		assert.Equal(t, "string", items.Properties[k].Type, k)
	}

	assert.Equal(t, "blocked", schema.If.Properties.Status.Const)
	assert.Equal(t, []string{"questions"}, schema.Then.Required)
	assert.Equal(t, 1, schema.Then.Properties.Questions.MinItems, "blocked asks at least one question")

	raw[0] = 'x'

	assert.True(t, json.Valid(prompts.ReadinessSchema()), "every call returns its own copy")
}

// finding is the shape of a finding in the review schemas.
type finding struct {
	Type       string
	Required   []string
	Properties struct {
		Severity         struct{ Enum []string }
		Where, What, Why struct{ Type string }
	}
}

func assertFinding(t *testing.T, f finding) {
	t.Helper()

	assert.Equal(t, "object", f.Type)
	assert.ElementsMatch(t, []string{"severity", "where", "what", "why"}, f.Required)
	assert.ElementsMatch(t, []string{"critical", "important", "minor"}, f.Properties.Severity.Enum)

	for _, p := range []struct{ Type string }{f.Properties.Where, f.Properties.What, f.Properties.Why} {
		assert.Equal(t, "string", p.Type)
	}
}

func TestReviewSchemaIsValidJSON(t *testing.T) {
	raw := prompts.ReviewSchema()
	require.True(t, json.Valid(raw))

	type verdict struct {
		Type       string
		Required   []string
		Properties struct {
			Pass   struct{ Type string }
			Reason struct{ Type string }
		}
	}

	var schema struct {
		Type       string
		Required   []string
		Properties struct {
			Asked, Built verdict
			Findings     struct {
				Type  string
				Items finding
			}
		}
	}

	require.NoError(t, json.Unmarshal(raw, &schema))
	assert.Equal(t, "object", schema.Type)
	assert.ElementsMatch(t, []string{"asked", "built", "findings"}, schema.Required)

	for _, v := range []verdict{schema.Properties.Asked, schema.Properties.Built} {
		assert.Equal(t, "object", v.Type)
		assert.ElementsMatch(t, []string{"pass", "reason"}, v.Required)
		assert.Equal(t, "boolean", v.Properties.Pass.Type)
		assert.Equal(t, "string", v.Properties.Reason.Type)
	}

	assert.Equal(t, "array", schema.Properties.Findings.Type)
	assertFinding(t, schema.Properties.Findings.Items)

	raw[0] = 'x'

	assert.True(t, json.Valid(prompts.ReviewSchema()), "every call returns its own copy")
}

func TestRereviewSchemaIsValidJSON(t *testing.T) {
	raw := prompts.RereviewSchema()
	require.True(t, json.Valid(raw))

	var schema struct {
		Type       string
		Required   []string
		Properties struct {
			Verdicts struct {
				Type  string
				Items struct {
					Type       string
					Required   []string
					Properties struct {
						Finding, Addressed, Evidence struct{ Type string }
					}
				}
			}
			Breakage struct {
				Type  string
				Items finding
			}
		}
	}

	require.NoError(t, json.Unmarshal(raw, &schema))
	assert.Equal(t, "object", schema.Type)
	assert.ElementsMatch(t, []string{"verdicts", "breakage"}, schema.Required)

	v := schema.Properties.Verdicts
	assert.Equal(t, "array", v.Type)
	assert.Equal(t, "object", v.Items.Type)
	assert.ElementsMatch(t, []string{"finding", "addressed", "evidence"}, v.Items.Required)
	assert.Equal(t, "integer", v.Items.Properties.Finding.Type)
	assert.Equal(t, "boolean", v.Items.Properties.Addressed.Type)
	assert.Equal(t, "string", v.Items.Properties.Evidence.Type)

	assert.Equal(t, "array", schema.Properties.Breakage.Type)
	assertFinding(t, schema.Properties.Breakage.Items)

	raw[0] = 'x'

	assert.True(t, json.Valid(prompts.RereviewSchema()), "every call returns its own copy")
}
