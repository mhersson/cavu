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
	tests := []struct {
		name   string
		data   any
		values []string // what the data must put into the prompt
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
			for _, wrong := range []any{nil, struct{ Other string }{}, map[string]string{}} {
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
