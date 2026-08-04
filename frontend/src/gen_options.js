import { getActiveSessionId } from './sessions';
import { getSessionGenOptions, setSessionGenOptions } from './api';

// Descriptor for each exposed Ollama generation parameter.
const GEN_PARAMS = [
    {
        key: 'num_ctx',
        label: 'Context window',
        type: 'int',
        min: 512,
        max: 131072,
        step: 512,
        placeholder: 'model default',
        hint: 'Max tokens in the context window (prompt + reply).',
    },
    {
        key: 'num_predict',
        label: 'Max tokens',
        type: 'int',
        min: -1,
        max: 32768,
        step: 1,
        placeholder: 'model default',
        hint: 'Max tokens to generate. −1 = unlimited.',
    },
    {
        key: 'temperature',
        label: 'Temperature',
        type: 'float',
        min: 0,
        max: 2,
        step: 0.05,
        placeholder: 'model default',
        hint: 'Sampling temperature. Higher = more creative, lower = more focused.',
    },
    {
        key: 'top_p',
        label: 'Top P',
        type: 'float',
        min: 0,
        max: 1,
        step: 0.01,
        placeholder: 'model default',
        hint: 'Nucleus sampling: consider only tokens whose cumulative probability ≥ this.',
    },
    {
        key: 'top_k',
        label: 'Top K',
        type: 'int',
        min: 0,
        max: 200,
        step: 1,
        placeholder: 'model default',
        hint: 'Consider only the top K most likely tokens. 0 = disabled.',
    },
    {
        key: 'repeat_penalty',
        label: 'Repeat penalty',
        type: 'float',
        min: 0.5,
        max: 2,
        step: 0.05,
        placeholder: 'model default',
        hint: 'Penalise recently used tokens to reduce repetition.',
    },
];

// Debounce timer for persisting changes.
let _saveTimer = null;

function scheduleGenOptionsSave(sessionId, getCurrentOpts) {
    clearTimeout(_saveTimer);
    _saveTimer = setTimeout(async () => {
        try {
            await setSessionGenOptions(sessionId, getCurrentOpts());
        } catch (err) {
            console.error('setSessionGenOptions', err);
        }
    }, 400);
}

// Renders the generation-options grid into #settingsGenOptions.
// Called by app.js whenever the settings lightbox opens, same pattern as
// renderToolsList().
export async function renderGenOptions() {
    const container = document.getElementById('settingsGenOptions');
    if (!container) return;

    container.innerHTML = '<div class="tools-panel-empty">Loading…</div>';

    const sessionId = getActiveSessionId();
    let current;
    try {
        current = await getSessionGenOptions(sessionId) ?? {};
    } catch (err) {
        console.error('getSessionGenOptions', err);
        container.innerHTML = '<div class="tools-panel-empty">Could not load options</div>';
        return;
    }

    container.innerHTML = '';

    // Build a live options object that the save closure reads.
    const opts = { ...current };

    for (const p of GEN_PARAMS) {
        const row = document.createElement('div');
        row.className = 'gen-opt-row';

        const labelEl = document.createElement('label');
        labelEl.className = 'gen-opt-label';
        labelEl.title = p.hint;
        labelEl.textContent = p.label;

        const input = document.createElement('input');
        input.type = 'number';
        input.className = 'gen-opt-input';
        input.min = p.min;
        input.max = p.max;
        input.step = p.step;
        input.placeholder = p.placeholder;
        if (opts[p.key] != null) input.value = opts[p.key];

        const clearBtn = document.createElement('button');
        clearBtn.className = 'gen-opt-clear';
        clearBtn.title = 'Reset to model default';
        clearBtn.textContent = '×';
        clearBtn.style.visibility = opts[p.key] != null ? 'visible' : 'hidden';

        input.addEventListener('input', () => {
            const raw = input.value.trim();
            if (raw === '') {
                delete opts[p.key];
                clearBtn.style.visibility = 'hidden';
            } else {
                const v = p.type === 'int' ? parseInt(raw, 10) : parseFloat(raw);
                if (!isNaN(v)) {
                    opts[p.key] = v;
                    clearBtn.style.visibility = 'visible';
                }
            }
            scheduleGenOptionsSave(sessionId, () => ({ ...opts }));
        });

        clearBtn.addEventListener('click', () => {
            input.value = '';
            delete opts[p.key];
            clearBtn.style.visibility = 'hidden';
            scheduleGenOptionsSave(sessionId, () => ({ ...opts }));
        });

        labelEl.htmlFor = `gen-opt-${p.key}`;
        input.id = `gen-opt-${p.key}`;

        row.appendChild(labelEl);
        row.appendChild(input);
        row.appendChild(clearBtn);
        container.appendChild(row);
    }
}
