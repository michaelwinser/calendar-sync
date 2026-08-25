package tools

import (
	"fmt"
	"net/http"

	"github.com/michaelwinser/appbase/auth"
)

// page renders the Tools HTML page.
func page(w http.ResponseWriter, r *http.Request) {
	email := auth.Email(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, toolsPage, email)
}

const toolsPage = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Calendar Sync - Tools</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            max-width: 800px;
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
        .form-row { display: flex; gap: 0.5rem; align-items: center; margin-bottom: 0.5rem; flex-wrap: wrap; }
        .form-row label { min-width: 80px; font-size: 0.9rem; }
        select, input[type="text"], input[type="date"] {
            padding: 0.3rem 0.5rem;
            border: 1px solid #ccc;
            border-radius: 4px;
            font-size: 0.9rem;
        }
        button {
            background: #e8e8e8;
            border: 1px solid #ccc;
            border-radius: 4px;
            padding: 0.4rem 1rem;
            cursor: pointer;
            font-size: 0.9rem;
        }
        button:hover { background: #ddd; }
        button:disabled { opacity: 0.5; cursor: default; }
        .btn-danger { background: #e74c3c; color: white; border-color: #c0392b; }
        .btn-danger:hover { background: #c0392b; }
        .btn-danger:disabled { background: #e74c3c; opacity: 0.5; }
        .event-list { margin: 0.75rem 0; }
        .event-item {
            display: flex;
            align-items: center;
            gap: 0.5rem;
            padding: 0.4rem 0;
            border-bottom: 1px solid #eee;
            font-size: 0.9rem;
        }
        .event-item:last-child { border-bottom: none; }
        .event-title { font-weight: 500; }
        .event-time { color: #666; font-size: 0.85rem; }
        .event-location { color: #888; font-size: 0.85rem; }
        .toolbar { display: flex; gap: 0.5rem; align-items: center; margin: 0.75rem 0; }
        #status { color: #666; font-size: 0.9rem; margin-left: 0.5rem; }
        .count { color: #666; font-size: 0.9rem; margin-bottom: 0.5rem; }
    </style>
</head>
<body>
    <h1>Tools</h1>
    <p class="meta">Signed in as %s &middot; <a href="/">Back to sync</a></p>

    <div class="section">
        <h2>Bulk Event Cleanup</h2>

        <div class="form-row">
            <label>Calendar</label>
            <select id="cal-select"><option value="">Loading...</option></select>
        </div>
        <div class="form-row">
            <label>From</label>
            <input type="date" id="date-from">
            <label style="min-width:auto">To</label>
            <input type="date" id="date-to">
        </div>
        <div class="form-row">
            <label>Title</label>
            <input type="text" id="title-filter" placeholder="Partial match (e.g. Busy)">
            <button onclick="searchEvents()">Search</button>
        </div>
        <div class="form-row">
            <label></label>
            <label style="min-width:auto;font-weight:normal"><input type="checkbox" id="sync-only"> Sync placeholders only (events created by calendar-sync)</label>
        </div>

        <div id="results"></div>
    </div>

    <script>
    let calendars = [];

    async function loadCalendars() {
        const res = await fetch('/api/calendars');
        if (!res.ok) return;
        calendars = await res.json();
        const sel = document.getElementById('cal-select');
        sel.innerHTML = '<option value="">— Select calendar —</option>' +
            calendars.map(c => '<option value="' + c.id + '">' + esc(c.name) +
                (c.primary ? ' (primary)' : '') + '</option>').join('');

        // Default date range: last 30 days to 60 days ahead
        const now = new Date();
        const from = new Date(now);
        from.setDate(from.getDate() - 30);
        const to = new Date(now);
        to.setDate(to.getDate() + 60);
        document.getElementById('date-from').value = fmtDate(from);
        document.getElementById('date-to').value = fmtDate(to);
    }

    async function searchEvents() {
        const calId = document.getElementById('cal-select').value;
        const from = document.getElementById('date-from').value;
        const to = document.getElementById('date-to').value;
        const q = document.getElementById('title-filter').value;
        const el = document.getElementById('results');

        if (!calId || !from || !to) {
            el.innerHTML = '<p class="error">Select a calendar and date range.</p>';
            return;
        }

        el.innerHTML = '<p>Searching...</p>';

        const syncOnly = document.getElementById('sync-only').checked;
        const params = new URLSearchParams({ calendarId: calId, timeMin: from, timeMax: to, q: q });
        if (syncOnly) params.set('syncOnly', 'true');
        const res = await fetch('/api/tools/search-events?' + params);
        if (!res.ok) {
            const err = await res.json().catch(() => ({}));
            el.innerHTML = '<p class="error">' + (err.error || 'Search failed') + '</p>';
            return;
        }

        const events = await res.json();
        if (events.length === 0) {
            el.innerHTML = '<p>No matching events found.</p>';
            return;
        }

        let html = '<p class="count">' + events.length + ' event(s) found</p>';
        html += '<div class="toolbar">';
        html += '<button onclick="toggleAll(true)">Select All</button>';
        html += '<button onclick="toggleAll(false)">Select None</button>';
        html += '<button class="btn-danger" id="delete-btn" onclick="deleteSelected()">Delete Selected</button>';
        html += '<span id="status"></span>';
        html += '</div>';
        html += '<div class="event-list">';
        for (const e of events) {
            const time = formatEventTime(e.start, e.end);
            html += '<div class="event-item">';
            html += '<input type="checkbox" checked value="' + e.id + '">';
            html += '<div>';
            html += '<span class="event-title">' + esc(e.summary || '(no title)') + '</span>';
            html += '<br><span class="event-time">' + time + '</span>';
            if (e.location) html += ' <span class="event-location">' + esc(e.location) + '</span>';
            html += '</div></div>';
        }
        html += '</div>';
        el.innerHTML = html;
    }

    function toggleAll(checked) {
        document.querySelectorAll('#results input[type="checkbox"]').forEach(cb => cb.checked = checked);
    }

    async function deleteSelected() {
        const calId = document.getElementById('cal-select').value;
        const checkboxes = document.querySelectorAll('#results input[type="checkbox"]:checked');
        const ids = Array.from(checkboxes).map(cb => cb.value);

        if (ids.length === 0) return;
        if (!confirm('Delete ' + ids.length + ' event(s)? This cannot be undone.')) return;

        const btn = document.getElementById('delete-btn');
        const status = document.getElementById('status');
        btn.disabled = true;
        status.textContent = 'Deleting...';

        const res = await fetch('/api/tools/delete-events', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ calendarId: calId, eventIds: ids })
        });
        const data = await res.json();
        status.textContent = data.message || 'Done';
        btn.disabled = false;

        // Re-search to update the list
        setTimeout(searchEvents, 1000);
    }

    function formatEventTime(start, end) {
        // All-day events have date format YYYY-MM-DD
        if (start.length === 10) return start + (end && end !== start ? ' – ' + end : '') + ' (all day)';
        const s = new Date(start);
        const e = new Date(end);
        const opts = { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' };
        return s.toLocaleString(undefined, opts) + ' – ' + e.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
    }

    function fmtDate(d) {
        return d.getFullYear() + '-' + String(d.getMonth()+1).padStart(2,'0') + '-' + String(d.getDate()).padStart(2,'0');
    }

    function esc(s) {
        const d = document.createElement('div');
        d.textContent = s;
        return d.innerHTML;
    }

    loadCalendars();
    </script>
</body>
</html>`
