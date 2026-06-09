#!/usr/bin/env node
// headersHelper for the memini MCP server. Claude Code runs this per connection
// and merges the JSON it prints over the static headers. We emit the
// cwd-resolved project namespace (the SAME resolver the hooks use, so capture
// and recall target one namespace) and a bearer token when one is configured —
// which is what makes a single remote memini work per-project.

import { resolveProject } from "./_shared.mjs";

const projectDir = process.env.CLAUDE_PROJECT_DIR || process.cwd();
const headers = { "X-Memini-Namespace": resolveProject(projectDir) };

const token = process.env.MEMINI_TOKEN || process.env.MEMINI_API_KEY;
if (token) headers.Authorization = `Bearer ${token}`;

process.stdout.write(JSON.stringify(headers));
