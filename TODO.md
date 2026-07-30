# To Do

## Features

- [ ] Add mermaid support to Markdown display
- [ ] Allow selection of database file using a command line switch (or config). Defaults to `sessions.db`.
- [ ] Move config file into config directory.
- [ ] Support for tool that queries the web using SearXNG or DuckDuckGo. See "Search Tool".
- [ ] Limited support for shell on Linux.
    - This requires careful planning
- [ ] Allow user to import artifacts to a session
- [ ] Allow user to enable / disable tools per session



## Search Tool

- User may specify which engine is preferred in config: SearNGX or DuckDuckGo (DDGS). Default to DDGS.
    - Confirm there is an equivalent to DDGS Python package available in Go.
    - If there is no DDGS, create something similar.
- Allow configuration of SearNGX location via the config file.
- If no SearNGX endpoint is specified, use the public directory to choose one at random.
- Search is one half of the tool. The other half is extraction.
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

