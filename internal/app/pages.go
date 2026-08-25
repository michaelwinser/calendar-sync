package app

import (
	"fmt"
	"net/http"

	"github.com/michaelwinser/appbase/auth"
)

// homeHandler renders the main sync UI. (The Tools page lives in internal/tools.)
func homeHandler(w http.ResponseWriter, r *http.Request) {
	email := auth.Email(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, homePage, email)
}

const homePage = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Calendar Sync</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            max-width: 640px;
            margin: 2rem auto;
            padding: 0 1rem;
            color: #1a1a1a;
        }
        h1 { margin-bottom: 0.5rem; }
        .meta { color: #666; margin-bottom: 2rem; }
        .section {
            background: #f7f7f7;
            border-radius: 8px;
            padding: 1.5rem;
            margin-bottom: 1rem;
        }
        .section h2 { font-size: 1rem; margin-bottom: 0.75rem; }
        .error { color: #c00; margin: 0.5rem 0; }
        .empty { color: #888; font-style: italic; }
        select {
            padding: 0.3rem 0.5rem;
            border: 1px solid #ccc;
            border-radius: 4px;
            font-size: 0.9rem;
            max-width: 100%%;
        }
        button, .btn {
            background: #e8e8e8;
            border: 1px solid #ccc;
            border-radius: 4px;
            padding: 0.4rem 1rem;
            cursor: pointer;
            font-size: 0.9rem;
        }
        button:hover, .btn:hover { background: #ddd; }
        button:disabled { opacity: 0.5; cursor: default; }
        .cal-list { list-style: none; margin-bottom: 0.75rem; }
        .cal-list li { padding: 0.3rem 0; }
        .cal-list label { cursor: pointer; }
        .cal-list input[disabled] + span { color: #999; }
        .color-swatch {
            display: inline-block;
            width: 20px; height: 20px;
            border-radius: 3px;
            cursor: pointer;
            position: relative;
        }
        .color-swatch:hover::after {
            content: attr(title);
            position: absolute;
            bottom: 100%%; left: 50%%;
            transform: translateX(-50%%);
            background: #333; color: white;
            padding: 2px 6px; border-radius: 3px;
            font-size: 0.75rem; white-space: nowrap;
            margin-bottom: 4px; z-index: 1;
        }
        .color-swatch input { display: none; }
        .color-text-opt {
            cursor: pointer;
            font-size: 0.8rem;
            padding: 2px 6px;
            border: 1px solid #ccc;
            border-radius: 3px;
            background: #f7f7f7;
        }
        .color-text-opt input { display: none; }
        .color-text-active { background: #ddd; border-color: #999; }
        .emoji-btn {
            cursor: pointer;
            font-size: 1.1rem;
            padding: 1px 3px;
            border-radius: 3px;
        }
        .emoji-btn:hover { background: #e0e0e0; }
        .hub-row { display: flex; gap: 0.5rem; align-items: center; margin-bottom: 0.5rem; }
        .hub-current { font-weight: 500; margin-bottom: 0.5rem; }
        #status { margin-top: 0.5rem; color: #666; font-size: 0.9rem; }
    </style>
</head>
<body>
    <h1>Calendar Sync</h1>
    <p class="meta">Signed in as %s</p>

    <div class="section">
        <h2>Hub Calendar</h2>
        <div id="hub-display"></div>
    </div>

    <div class="section">
        <h2>Sync Calendars</h2>
        <div id="sources-display"></div>
    </div>

    <div class="section">
        <h2>Sync Settings</h2>
        <div style="display:flex;gap:1rem;align-items:center;margin-bottom:0.5rem;flex-wrap:wrap">
            <label style="font-size:0.9rem">Window:
                <select id="sync-window" onchange="saveSettings()">
                    <option value="2">2 weeks</option>
                    <option value="4">4 weeks</option>
                    <option value="6">6 weeks</option>
                    <option value="8" selected>8 weeks</option>
                    <option value="12">12 weeks</option>
                </select>
            </label>
            <label style="font-size:0.9rem">Auto-sync interval:
                <select id="sync-interval" onchange="saveSettings()">
                    <option value="5">5 min</option>
                    <option value="10">10 min</option>
                    <option value="15" selected>15 min</option>
                    <option value="30">30 min</option>
                    <option value="60">1 hour</option>
                </select>
            </label>
        </div>
    </div>

    <div class="section">
        <h2>Sync</h2>
        <div>
            <button id="sync-btn" onclick="runSync()">Sync Now</button>
            <span id="sync-status"></span>
        </div>
        <div id="sync-logs" style="margin-top:0.75rem"></div>
    </div>

    <div>
        <button onclick="signOut()">Sign out</button>
        <a href="/tools" style="margin-left:1rem;font-size:0.9rem">Tools</a>
        <span id="status"></span>
    </div>

    <script>
    let calendars = [];
    let config = { hubCalendarId: '', hubCalendarName: '', syncWindowWeeks: 8, syncIntervalMinutes: 15, sources: [] };

    async function load() {
        try {
            const [calRes, cfgRes] = await Promise.all([
                fetch('/api/calendars'),
                fetch('/api/config')
            ]);
            if (!calRes.ok) {
                document.getElementById('hub-display').innerHTML =
                    '<p class="error">Could not load calendars. Try signing out and back in.</p>';
                return;
            }
            calendars = await calRes.json();
            if (cfgRes.ok) config = await cfgRes.json();
            initLocalState();
            render();
        } catch (e) {
            document.getElementById('hub-display').innerHTML =
                '<p class="error">Failed to connect to server.</p>';
        }
    }

    function render() {
        renderHub();
        renderSources();
        renderSettings();
    }

    function renderSettings() {
        const windowSel = document.getElementById('sync-window');
        const intervalSel = document.getElementById('sync-interval');
        if (windowSel) windowSel.value = config.syncWindowWeeks || 8;
        if (intervalSel) intervalSel.value = config.syncIntervalMinutes || 15;
    }

    async function saveSettings() {
        config.syncWindowWeeks = parseInt(document.getElementById('sync-window').value) || 8;
        config.syncIntervalMinutes = parseInt(document.getElementById('sync-interval').value) || 15;
        saveConfig();
    }

    function renderHub() {
        const el = document.getElementById('hub-display');
        const options = calendars
            .map(c => '<option value="' + c.id + '"' +
                (c.id === config.hubCalendarId ? ' selected' : '') +
                '>' + esc(c.name) + (c.primary ? ' (primary)' : '') + '</option>')
            .join('');

        let html = '';
        if (config.hubCalendarId) {
            html += '<p class="hub-current">Current: ' + esc(config.hubCalendarName) + '</p>';
        }
        html += '<div class="hub-row">';
        html += '<select id="hub-select"><option value="">— Select hub calendar —</option>' + options + '</select>';
        html += '<button onclick="setHub()">Set Hub</button>';
        html += '</div>';
        el.innerHTML = html;
    }

    const colorOptions = [
        {id: '', label: 'Same as destination', color: ''},
        {id: 'source', label: 'Source calendar color', color: ''},
        {id: '1', label: 'Lavender', color: '#7986cb'},
        {id: '2', label: 'Sage', color: '#33b679'},
        {id: '3', label: 'Grape', color: '#8e24aa'},
        {id: '4', label: 'Flamingo', color: '#e67c73'},
        {id: '5', label: 'Banana', color: '#f6bf26'},
        {id: '6', label: 'Tangerine', color: '#f4511e'},
        {id: '7', label: 'Peacock', color: '#039be5'},
        {id: '8', label: 'Graphite', color: '#616161'},
        {id: '9', label: 'Blueberry', color: '#3f51b5'},
        {id: '10', label: 'Basil', color: '#0b8043'},
        {id: '11', label: 'Tomato', color: '#d50000'},
    ];

    // Track locally checked calendars so options show before Apply
    let localChecked = new Set();
    let localOptions = {}; // calId -> {emojiPrefix, colorId}

    function initLocalState() {
        localChecked = new Set(config.sources.map(s => s.calendarId));
        localOptions = {};
        config.sources.forEach(s => {
            localOptions[s.calendarId] = { emojiPrefix: s.emojiPrefix || '', colorId: s.colorId || '' };
        });
    }

    function setPrefix(calId, emoji) {
        const input = document.getElementById('prefix-' + calId);
        if (input) input.value = emoji;
    }

    function onCheckboxChange(calId, isChecked) {
        // Save current input values before re-render
        saveLocalInputs();
        if (isChecked) {
            localChecked.add(calId);
            if (!localOptions[calId]) localOptions[calId] = { emojiPrefix: '', colorId: '' };
        } else {
            localChecked.delete(calId);
        }
        renderSources();
    }

    function saveLocalInputs() {
        for (const calId of localChecked) {
            const prefixInput = document.querySelector('input[data-cal="' + calId + '"][data-field="emoji"]');
            const colorRadio = document.querySelector('input[name="color-' + calId + '"]:checked');
            if (prefixInput || colorRadio) {
                localOptions[calId] = {
                    emojiPrefix: prefixInput ? prefixInput.value : '',
                    colorId: colorRadio ? colorRadio.value : '',
                };
            }
        }
    }

    function renderSources() {
        const el = document.getElementById('sources-display');

        const available = calendars.filter(c => c.id !== config.hubCalendarId);
        if (available.length === 0) {
            el.innerHTML = '<p class="empty">No calendars available. Set a hub calendar first.</p>';
            return;
        }

        let html = '<ul class="cal-list">';
        for (const cal of available) {
            const isChecked = localChecked.has(cal.id);
            const checked = isChecked ? ' checked' : '';
            const label = esc(cal.name) + (cal.primary ? ' (primary)' : '');
            const opts = localOptions[cal.id] || { emojiPrefix: '', colorId: '' };

            html += '<li style="padding:0.4rem 0">';
            html += '<label><input type="checkbox" value="' + cal.id + '"' + checked +
                ' data-name="' + esc(cal.name) + '"' +
                ' onchange="onCheckboxChange(\'' + cal.id + '\', this.checked)"' +
                '> <span>' + label + '</span></label>';

            if (isChecked) {
                html += '<div style="margin-left:1.5rem;margin-top:0.3rem;font-size:0.85rem">';

                // Prefix input with emoji quick-pick
                html += '<div style="margin-bottom:0.3rem;display:flex;align-items:center;gap:0.3rem">';
                html += '<label>Prefix: <input type="text" id="prefix-' + cal.id + '" data-cal="' + cal.id + '" data-field="emoji" value="' +
                    esc(opts.emojiPrefix) + '" style="width:8rem;padding:0.2rem" placeholder="e.g. [Sync]"></label>';
                var emojis = [
                    {e: '🔄', t: 'Sync'},
                    {e: '🔁', t: 'Repeat'},
                    {e: '📅', t: 'Calendar'},
                    {e: '📌', t: 'Pin'},
                    {e: '🔗', t: 'Link'},
                    {e: '⏳', t: 'Hourglass'},
                    {e: '🪞', t: 'Mirror'},
                    {e: '📋', t: 'Clipboard'},
                ];
                for (var ei of emojis) {
                    html += '<span class="emoji-btn" title="' + ei.t + '" onclick="setPrefix(\'' + cal.id + '\',\'' + ei.e + '\')">' + ei.e + '</span>';
                }
                html += '</div>';

                // Color picker: swatches only, tooltip on hover
                html += '<div style="display:flex;gap:0.3rem;align-items:center;flex-wrap:wrap">';
                html += '<span>Color: </span>';
                for (const c of colorOptions) {
                    const isChecked = opts.colorId === c.id;
                    const checkedStyle = isChecked ? 'outline:2px solid #333;outline-offset:1px;' : '';
                    if (c.color) {
                        html += '<span class="color-swatch" style="background:' + c.color + ';' + checkedStyle + '" title="' + c.label + '">' +
                            '<input type="radio" name="color-' + cal.id + '" value="' + c.id + '"' + (isChecked ? ' checked' : '') +
                            ' data-cal="' + cal.id + '" data-field="color"></span>';
                    } else {
                        // Text options (default, source)
                        html += '<label class="color-text-opt' + (isChecked ? ' color-text-active' : '') + '" title="' + c.label + '">' +
                            '<input type="radio" name="color-' + cal.id + '" value="' + c.id + '"' + (isChecked ? ' checked' : '') +
                            ' data-cal="' + cal.id + '" data-field="color">' +
                            '<span>' + (c.id === '' ? 'Dest' : 'Src') + '</span></label>';
                    }
                }
                html += '</div>';

                html += '</div>';
            }

            html += '</li>';
        }
        html += '</ul>';
        html += '<button onclick="applySources()">Apply</button>';

        el.innerHTML = html;
    }

    async function saveConfig() {
        setStatus('Saving...');
        const res = await fetch('/api/config', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(config)
        });
        if (!res.ok) {
            const err = await res.json().catch(() => ({}));
            setStatus('Error: ' + (err.error || 'save failed'));
            return false;
        }
        config = await res.json();
        initLocalState();
        render();
        setStatus('Saved.');
        return true;
    }

    function setHub() {
        const sel = document.getElementById('hub-select');
        if (!sel.value) return;
        const cal = calendars.find(c => c.id === sel.value);
        if (!cal) return;

        config.sources = config.sources.filter(s => s.calendarId !== cal.id);
        config.hubCalendarId = cal.id;
        config.hubCalendarName = cal.name;
        saveConfig();
    }

    function applySources() {
        saveLocalInputs();
        config.sources = [];
        const checkboxes = document.querySelectorAll('#sources-display input[type="checkbox"]');
        checkboxes.forEach(cb => {
            if (cb.checked) {
                const calId = cb.value;
                const opts = localOptions[calId] || { emojiPrefix: '', colorId: '' };
                config.sources.push({
                    calendarId: calId,
                    calendarName: cb.dataset.name,
                    emojiPrefix: opts.emojiPrefix,
                    colorId: opts.colorId,
                });
            }
        });
        saveConfig();
    }

    function signOut() {
        fetch('/api/auth/logout', { method: 'POST' }).then(() => location.reload());
    }

    function setStatus(msg) {
        document.getElementById('status').textContent = msg;
        if (msg === 'Saved.') setTimeout(() => setStatus(''), 2000);
    }

    function esc(s) {
        const d = document.createElement('div');
        d.textContent = s;
        return d.innerHTML;
    }

    async function runSync() {
        const btn = document.getElementById('sync-btn');
        const status = document.getElementById('sync-status');
        btn.disabled = true;
        status.textContent = 'Syncing...';
        try {
            const res = await fetch('/api/sync', { method: 'POST' });
            const text = await res.text();
            let data;
            try { data = JSON.parse(text); } catch { data = null; }
            if (!res.ok) {
                status.textContent = 'Error: ' + (data?.error || text || 'sync failed');
            } else if (data) {
                status.textContent = data.message;
            } else {
                status.textContent = text;
            }
            loadSyncLogs();
        } catch (e) {
            status.textContent = 'Error: ' + e.message;
        }
        btn.disabled = false;
    }

    async function loadSyncLogs() {
        const el = document.getElementById('sync-logs');
        try {
            const res = await fetch('/api/sync/logs');
            if (!res.ok) return;
            const logs = await res.json();
            if (logs.length === 0) {
                el.innerHTML = '<p class="empty">No sync history yet.</p>';
                return;
            }
            let html = '<table style="width:100%%;font-size:0.85rem;border-collapse:collapse">';
            html += '<tr style="text-align:left;border-bottom:1px solid #ddd"><th>Time</th><th>Result</th><th>Status</th></tr>';
            for (const log of logs.slice(0, 10)) {
                const t = new Date(log.startedAt).toLocaleString();
                const counts = log.created + ' new, ' + log.updated + ' upd, ' + log.deleted + ' del';
                const s = log.errors > 0 ? counts + ', ' + log.errors + ' err' : counts;
                html += '<tr style="border-bottom:1px solid #eee"><td>' + t + '</td><td>' + s + '</td><td>' + log.status + '</td></tr>';
                // Per-calendar breakdown
                if (log.details) {
                    try {
                        const details = typeof log.details === 'string' ? JSON.parse(log.details) : log.details;
                        for (const [cal, d] of Object.entries(details)) {
                            if (d.created || d.updated || d.deleted) {
                                const ds = d.created + ' new, ' + d.updated + ' upd, ' + d.deleted + ' del';
                                html += '<tr style="border-bottom:1px solid #eee;color:#888"><td style="padding-left:1rem">' + esc(cal) + '</td><td>' + ds + '</td><td></td></tr>';
                            }
                        }
                    } catch(e) {}
                }
                // Error details
                if (log.errorDetails) {
                    try {
                        const errs = typeof log.errorDetails === 'string' ? JSON.parse(log.errorDetails) : log.errorDetails;
                        for (const e of errs) {
                            html += '<tr style="border-bottom:1px solid #eee;color:#c00"><td colspan="3" style="padding-left:1rem;font-size:0.8rem">' + esc(e) + '</td></tr>';
                        }
                    } catch(e) {}
                }
            }
            html += '</table>';
            el.innerHTML = html;
        } catch (e) {}
    }

    load();
    loadSyncLogs();
    </script>
</body>
</html>`
