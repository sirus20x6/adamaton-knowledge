package extract

import "strings"

// promptTemplate asks the model for a JSON object matching the schema
// enforced by vLLM's guided_json. The prior XML-tag prompt was a
// near-verbatim port of R2R's graph_extraction.yaml but qwen3.x
// routinely ignored the schema (Markdown bullets, hallucinated tags,
// repetitive degeneration on short inputs). Schema-enforced JSON is
// the only thing that survives a fp8-quant 27B model reliably.
//
// We keep one shorter few-shot to anchor entity granularity (otherwise
// the model emits 3-4 generic entities per chunk).
const promptTemplate = `# Goal
Extract entities and their relationships from the text below.

# Output
Return a single JSON object with two keys:
- "entities": array of {name, type, description}
- "relationships": array of {source, target, relation, description, weight}

Field guidance:
- name: short canonical name, capitalized (e.g., "BERT", "California", "Google Brain")
- type: short noun category (e.g., "Model", "State", "Organization", "Dataset", "Benchmark", "Task", "Method")
- description: one sentence grounded in the text
- source/target: must each match an entity name from your entities array
- relation: short verb phrase (e.g., "Located In", "Evaluates On", "Built By")
- weight: 0-10 integer-ish strength of the relation

Extract every distinct entity that appears or is meaningfully referenced. Include people, organizations, models, datasets, benchmarks, methods, places, and concepts. Prefer 8-20 entities for a typical paper section over 3-4 generic ones. Every entity should appear in at least one relationship.

# Example
Text: "San Francisco is a city in California known for the Golden Gate Bridge."

Output:
{"entities":[{"name":"San Francisco","type":"City","description":"A city in California known for the Golden Gate Bridge."},{"name":"California","type":"State","description":"A US state."},{"name":"Golden Gate Bridge","type":"Landmark","description":"A famous bridge in San Francisco."}],"relationships":[{"source":"San Francisco","target":"California","relation":"Located In","description":"San Francisco is located in California.","weight":8},{"source":"San Francisco","target":"Golden Gate Bridge","relation":"Features","description":"San Francisco features the Golden Gate Bridge.","weight":9}]}

# Text to Extract From
`

// buildPrompt renders the full prompt body. We append the input text
// then a JSON-shape footer that anchors the model to start with "{".
func buildPrompt(body string) string {
	var sb strings.Builder
	sb.WriteString(promptTemplate)
	sb.WriteString(body)
	sb.WriteString("\n\n# JSON Output\n")
	return sb.String()
}
