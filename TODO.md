# To Do

## Display & User Interface

- [x] Timer should not appear in status bar when condition is "Ready". It should only appear when waiting.
- [x] Add mermaid support to Markdown display
- [x] BUG: Time displayed at the top of messages is the current time, not the timestamp of the message.
- [x] Improve context size indicator. Is there anything returned from Ollama that can yield a more accurate number? The current one appears to be grossly inaccurate.


## Features

- [x] Allow selection of database file using a command line switch (or config). Defaults to `sessions.db`.
- [x] Allow user to import artifacts to a session
- [x] Allow user to enable / disable tools per session
- [x] Move config file into config directory.
- [x] Support for tool that queries the web using SearXNG or DuckDuckGo. See "Search Tool" section.
- [x] Update skills to support more rich structure. See "Skills Upgrade" section.
- [x] During a plan loop, tool calls should not be removed from the context until the end. Many of them are to pull additional info into the context.
- [ ] Tool to allow limited shell access. See "Shell Tool" section.


## Skills Upgrade

Today, skills are presented as simple Markdown files. This upgrade should allow for more complex skill setups and is the first of several upgrades planned. This should be a simple change that will be built upon in future updates.

- Existing Markdown skills remain unchanged.
- Support for "rich" skills which are a directory containing a `SKILL.md` file. This file is formatted the same way skills are currently, with `name` and `description` in the frontmatter.
- The `SKILL.md` file may reference other files in the same directory. A subsequent call to the `skill` tool should allow the LLM to fetch one or more of the referenced files.


## Search Tool

- User may specify which engine is preferred in config: SearNGX or DuckDuckGo (DDGS). Default to DDGS.
    - Confirm there is an equivalent to DDGS Python package available in Go.
    - If there is no DDGS, create something similar.
- Allow configuration of SearNGX location via the config file.
- If no SearNGX endpoint is specified, use the public directory to choose one at random.
- Search is one half of the tool. The other (bigger) half is extraction.
    - Tool call that can pull the content from a URL (the initial search provides URL).
    - Tool must fetch the URL content with a User-Agent string that looks like a real browser
    - Tool should discard advertising-related content from the retrieved page
    - Tool must perform page fetching in an invisible sandboxed iframe
        - Page may run scripts to fetch content
        - There should be a small delay before extraction to allow page to settle
    - Tool should return the page content as formatted Markdown.
        - I have an example from a Chrome plugin that transforms most pages to Markdown already
        - We can adapt something from that plugin's source
    - Tool should cache the result
    - Tool should use deterministic methods (not LLM)


## Shell Tool

- Will require careful planning as well as an (optional) user-approval process for the tool call.
    - Determine security boundaries
    - Scan commands deterministically for dangerous patterns
- User approval requirement should default to "on" per-session. User may opt-in to "auto".
- Bash on Linux, Powershell on Windows
- LLM may pair this with file create tool call in order to generate and execute scripts
