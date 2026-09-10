// Package llm is loop's provider abstraction: one Provider interface
// (Complete + Stream) that normalizes messages, tools, and structured
// output across the Anthropic Messages API and the OpenAI API shape
// (including base_url-compatible services), plus a deterministic mock
// provider used by the whole test suite.
package llm
