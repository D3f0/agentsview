# /// script
# requires-python = ">=3.11"
# dependencies = ["marimo>=0.23.16", "anywidget", "traitlets"]
# ///

import marimo

__generated_with = "0.23.16"
app = marimo.App(width="medium")


@app.cell
def _():
    import marimo as mo
    import anywidget
    import os
    import sqlite3
    import traitlets
    from pathlib import Path

    data_dir = Path(os.environ.get("AGENTSVIEW_DATA_DIR", Path.home() / ".agentsview"))
    connection = sqlite3.connect((data_dir / "sessions.db").resolve().as_uri() + "?mode=ro", uri=True)
    connection.row_factory = sqlite3.Row
    sessions = [dict(row) for row in connection.execute(
        """
        WITH message_totals AS (
            SELECT session_id, GROUP_CONCAT(DISTINCT NULLIF(model, '')) AS models,
                   SUM(context_tokens) AS context_tokens, SUM(output_tokens) AS output_tokens
            FROM messages WHERE role = 'assistant' GROUP BY session_id
        ), usage_totals AS (
            SELECT session_id,
                   SUM(input_tokens + cache_creation_input_tokens + cache_read_input_tokens) AS input_tokens,
                   SUM(output_tokens) AS normalized_output_tokens,
                   SUM(reasoning_tokens) AS reasoning_tokens,
                   SUM(cost_microdollars) / 1000000.0 AS recorded_cost_usd
            FROM usage_events GROUP BY session_id
        ), tools AS (
            SELECT session_id, COUNT(*) AS tool_calls, GROUP_CONCAT(DISTINCT category) AS tool_categories
            FROM tool_calls GROUP BY session_id
        )
        SELECT s.id, s.agent AS harness, COALESCE(NULLIF(s.entrypoint, ''), 'default') AS entrypoint,
               s.project, s.first_message, s.started_at, s.outcome, s.health_grade, s.health_score,
               COALESCE(m.models, '') AS models, COALESCE(m.context_tokens, 0) AS context_tokens,
               COALESCE(m.output_tokens, 0) AS output_tokens, COALESCE(u.input_tokens, 0) AS input_tokens,
               COALESCE(u.normalized_output_tokens, 0) AS normalized_output_tokens,
               COALESCE(u.reasoning_tokens, 0) AS reasoning_tokens, u.recorded_cost_usd,
               COALESCE(t.tool_calls, 0) AS tool_calls, COALESCE(t.tool_categories, '') AS tool_categories
        FROM sessions AS s
        LEFT JOIN message_totals AS m ON m.session_id = s.id
        LEFT JOIN usage_totals AS u ON u.session_id = s.id
        LEFT JOIN tools AS t ON t.session_id = s.id
        ORDER BY s.started_at DESC
        """
    )]
    connection.close()

    class SessionBrowser(anywidget.AnyWidget):
        _esm = "function render({ model, el }) { const fmt = new Intl.NumberFormat(); const root = document.createElement('section'); root.className = 'session-browser'; const search = document.createElement('input'); search.placeholder = 'Filter harness, project, model, or prompt'; const list = document.createElement('select'); list.size = 12; const detail = document.createElement('pre'); function visible() { const term = search.value.toLowerCase(); return (model.get('sessions') || []).filter((row) => JSON.stringify(row).toLowerCase().includes(term)); } function updateDetail() { const row = (model.get('sessions') || []).find((item) => item.id === model.get('selected_id')); if (!row) { detail.textContent = 'Select a session to inspect its telemetry.'; return; } const cost = row.recorded_cost_usd == null ? 'not recorded' : '$' + row.recorded_cost_usd.toFixed(4); detail.textContent = [row.harness + ' · ' + row.entrypoint + ' · ' + (row.project || 'no project'), 'Models: ' + (row.models || 'not recorded'), 'Message context tokens: ' + fmt.format(row.context_tokens), 'Message output tokens: ' + fmt.format(row.output_tokens), 'Normalized input tokens: ' + fmt.format(row.input_tokens), 'Normalized output tokens: ' + fmt.format(row.normalized_output_tokens), 'Reasoning tokens: ' + fmt.format(row.reasoning_tokens), 'Recorded cost: ' + cost, 'Tool calls: ' + fmt.format(row.tool_calls) + ' (' + (row.tool_categories || 'none') + ')', 'Outcome / health: ' + (row.outcome || 'unknown') + ' / ' + ([row.health_grade, row.health_score].filter(Boolean).join(' ') || 'not scored'), '', row.first_message || 'No initial prompt recorded.', '', row.id].join(String.fromCharCode(10)); } function renderList() { const rows = visible(); list.replaceChildren(); rows.slice(0, 500).forEach((row) => { const option = document.createElement('option'); option.value = row.id; option.textContent = row.harness + ' · ' + (row.project || '—') + ' · ' + (row.models || 'model unavailable') + ' · ' + fmt.format(row.context_tokens) + ' context'; list.add(option); }); if (model.get('selected_id')) list.value = model.get('selected_id'); updateDetail(); } search.addEventListener('input', renderList); list.addEventListener('change', () => { model.set('selected_id', list.value); model.save_changes(); updateDetail(); }); model.on('change:selected_id', updateDetail); root.append(search, list, detail); el.append(root); renderList(); } export default { render };"
        _css = """.session-browser { display:grid; grid-template-columns:minmax(320px,1fr) minmax(360px,1.3fr); gap:12px; min-height:420px; font:13px ui-sans-serif,system-ui,sans-serif; } .session-browser input { grid-column:1/-1; padding:9px 11px; border:1px solid #b9c8dc; border-radius:8px; } .session-browser select { min-height:360px; border:1px solid #b9c8dc; border-radius:8px; padding:5px; background:#f8fbff; } .session-browser pre { margin:0; padding:14px; white-space:pre-wrap; overflow:auto; background:#f3f7fd; border:1px solid #d7e2f0; border-radius:8px; color:#1e293b; } @media (prefers-color-scheme:dark) { .session-browser input,.session-browser select { background:#172133;color:#e5edf8;border-color:#40516c; } .session-browser pre { background:#142035;color:#e5edf8;border-color:#374a67; } }"""
        sessions = traitlets.List(traitlets.Dict()).tag(sync=True)
        selected_id = traitlets.Unicode("").tag(sync=True)

    session_browser = mo.ui.anywidget(SessionBrowser(sessions=sessions))
    session_browser
    return


if __name__ == "__main__":
    app.run()
