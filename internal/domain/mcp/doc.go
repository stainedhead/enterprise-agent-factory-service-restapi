// Package mcp is the pure core of governed MCP servers (mcp-gateway-PRD.md, release 1.3.0): server and
// primitive names, the canonical JSON digest used to pin definitions (RFC 8785), the policy that decides
// which tools, resources and prompts an agent may see and call, the risk tier and automatic description
// checks of team-registered servers, the team server approval policy, and the keyword (BM25) search
// index behind the gateway's afs_search meta-tool. It imports the standard library only.
package mcp
