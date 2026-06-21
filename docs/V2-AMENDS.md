# V2 Amends

1. We can’t use Vertex AI with Workload Identity: let’s use OpenAI’s Responses API via Workload Identity Federation authentication instead.
2. The current financial budgeting implementation appears quite broken: for instance we hard-code costs returned for every research session. Let’s simplify V2 by deferring all finance/budgeting fixes (token or financial) out to a future release.
3. Additionally, on Vertex, the fallback to SSE is broken as well. If we’re leveraging OpenAI’s Responses or Anthropic’s Messages API, we can revert to long requests, with future support for webhooks. This slots nicely into Gemini’s Interactions API too, which also has support for webhooks.
4. Use Buf to generate a proto client, rather than importing types (or ideally stirrup) via `go mod`
5. The research submissions API is underspecified: let’s use ConnectRPC over neat gRPC (like Stirrup does) because we may choose to add a UI to Chiron in future.
6. Focus on just external (internet) research right now, we’ll push weaving internal sources (via `file_search` or MCP) into a future version. V2 just needs to produce the same sort of content as Gemini’s research agents currently produce, we’ll focus on tuning/internal context later, once Paddock is fully online to enable this.
7. As we now have the potential to spend a _lot_ of real money on tokens, our observability needs to be much, much stronger than it currently is. Include explicit CLI options for forwarding OTLP telemetry to Langfuse, so we can leverage Langfuse to understand/monitor/finesse Chiron’s capabilities.