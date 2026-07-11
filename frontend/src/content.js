import { marked } from 'marked';
import mathMarked from "@webc.site/math-marked";

import hljs from 'highlight.js/lib/core';
import 'highlight.js/styles/atom-one-dark.css';

// Register a useful set of languages to keep bundle size small.
import javascript from 'highlight.js/lib/languages/javascript';
import typescript from 'highlight.js/lib/languages/typescript';
import python from 'highlight.js/lib/languages/python';
import bash from 'highlight.js/lib/languages/bash';
import go from 'highlight.js/lib/languages/go';
import rust from 'highlight.js/lib/languages/rust';
import java from 'highlight.js/lib/languages/java';
import cpp from 'highlight.js/lib/languages/cpp';
import sql from 'highlight.js/lib/languages/sql';
import html from 'highlight.js/lib/languages/xml';
import xml from 'highlight.js/lib/languages/xml';
import json from 'highlight.js/lib/languages/json';
import yaml from 'highlight.js/lib/languages/yaml';
import markdown from 'highlight.js/lib/languages/markdown';
import css from 'highlight.js/lib/languages/css';
import dart from 'highlight.js/lib/languages/dart';

hljs.registerLanguage('javascript', javascript);
hljs.registerLanguage('typescript', typescript);
hljs.registerLanguage('python', python);
hljs.registerLanguage('bash', bash);
hljs.registerLanguage('sh', () => bash);
hljs.registerLanguage('shell', () => bash);
hljs.registerLanguage('go', go);
hljs.registerLanguage('golang', () => go);
hljs.registerLanguage('rust', rust);
hljs.registerLanguage('java', java);
hljs.registerLanguage('cpp', cpp);
hljs.registerLanguage('c++', () => cpp);
hljs.registerLanguage('sql', sql);
hljs.registerLanguage('html', html);
hljs.registerLanguage('xml', xml);
hljs.registerLanguage('json', json);
hljs.registerLanguage('yaml', yaml);
hljs.registerLanguage('md', () => markdown);
hljs.registerLanguage('markdown', markdown);
hljs.registerLanguage('css', css);
hljs.registerLanguage('dart', dart);

// Configure marked: GFM mode, no line breaks on single newlines (LLM-friendly)
marked.use(mathMarked());
marked.setOptions({ gfm: true, breaks: false });


// Simple HTML escape to avoid XSS in user input.
export function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
}

// Syntax-highlight a raw text blob as a given language, reusing the same
// hljs instance/registered languages as renderMarkdown's code blocks. Falls
// back to escaped plain text if highlighting fails or the language isn't
// registered. Used by the tool-call lightbox (see app.js) for its
// arguments/result panes.
export function renderHighlighted(text, lang) {
    try {
        if (lang && hljs.getLanguage(lang)) {
            return hljs.highlight(text, { language: lang }).value;
        }
    } catch {}
    return escapeHtml(text);
}

// Render markdown with fenced code blocks highlighted by lang.
export function renderMarkdown(text) {
    // Use marked.parseSync so it returns a string (not void).
    const html = marked.parse(text);

    // Post-process fenced <pre><code> blocks: add a language label + copy button
    const tmp = document.createElement('div');
    tmp.innerHTML = html;

    for (const codeEl of tmp.querySelectorAll('pre code')) {
        const pre = codeEl.parentElement;
        if (!pre || pre.tagName !== 'PRE') continue;

        // Extract language class from marked's output (e.g. "language-js")
        let lang = '';
        codeEl.classList.forEach(cls => {
            if (cls.startsWith('language-')) {
                lang = cls.replace('language-', '');
            }
        });


        const rawText = codeEl.textContent;
        try {
            if(lang) {
                const result = hljs.highlight(rawText, {language: lang});
                codeEl.innerHTML = result.value;
                codeEl.className = `hljs language-${lang}`;
            } else {
                const detected = hljs.highlightAuto(rawText);
                if(detected.language && !['no-highlight','plaintext'].includes(detected.language)) {
                    lang = detected.language;
                    codeEl.innerHTML = detected.value;
                    codeEl.className = `hljs language-${lang}`;
                } else {
                    codeEl.className = '';
                }
            }
        } catch{}

        const wrapper = document.createElement('div');
        wrapper.className = 'code-block';
        pre.parentNode?.replaceChild(wrapper, pre);
        wrapper.appendChild(pre); // keep the <pre><code> inside

        if (lang) {
            const label = document.createElement('span');
            label.className = 'code-lang-label';
            label.textContent = lang;
            wrapper.insertBefore(label, pre);
        }

        
        // Copy button
        const copyBtn = document.createElement('button');
        copyBtn.className = 'code-copy-btn';
        copyBtn.textContent = 'copy';
        /*
        copyBtn.addEventListener('click', () => {
            navigator.clipboard.writeText(codeEl.textContent).then(() => {
                copyBtn.textContent = 'copied!';
                setTimeout(() => (copyBtn.textContent = 'copy'), 1500);
            });
        });
        */
        wrapper.insertBefore(copyBtn, pre); // between label and <pre>
        
    }

    return tmp.innerHTML;
}
