#!/usr/bin/env python3
"""Generate a local, self-contained HTML call sheet from an enriched-cleaned CSV.

Usage: python3 gen_callsheet.py <cleaned.csv> <output.html>

The output HTML embeds all data inline (no external requests, no CDN) so
it's safe to open directly from disk. It is never meant to be published
publicly — the data includes real names, phone numbers, and emails.
"""
import csv
import hashlib
import json
import sys
import html
from pathlib import Path

# Abstract results are read straight from the on-disk cache (populated by
# `go run . -in enriched.csv -validate-phones`) rather than from CSV
# columns, so every phone number can carry its own validation badge
# without adding a validation column per phone to the CSV.
ABSTRACT_CACHE_DIR = Path(__file__).parent / ".cache" / "abstract"


def abstract_lookup(phone):
    if not phone:
        return None
    cache_path = ABSTRACT_CACHE_DIR / f"{hashlib.sha1(phone.encode()).hexdigest()}.json"
    if not cache_path.exists():
        return None
    try:
        data = json.loads(cache_path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return None
    return {
        "valid": data.get("phone_validation", {}).get("is_valid"),
        "lineStatus": data.get("phone_validation", {}).get("line_status", ""),
        "lineType": data.get("phone_carrier", {}).get("line_type", ""),
        "isVoip": data.get("phone_validation", {}).get("is_voip", False),
    }


def build_properties(rows):
    properties = []
    for row in rows:
        persons = []
        for p in (1, 2, 3):
            name = row.get(f"batchdata_person{p}_name", "").strip()
            if not name:
                continue
            phones = []
            for ph in (1, 2, 3, 4, 5):
                num = row.get(f"batchdata_person{p}_phone{ph}_number", "").strip()
                if not num:
                    continue
                phones.append({
                    "number": num,
                    "type": row.get(f"batchdata_person{p}_phone{ph}_type", ""),
                    "tested": row.get(f"batchdata_person{p}_phone{ph}_tested", "") == "true",
                    "reachable": row.get(f"batchdata_person{p}_phone{ph}_reachable", "") == "true",
                    "dnc": row.get(f"batchdata_person{p}_phone{ph}_dnc", "") == "true",
                    "abstract": abstract_lookup(num),
                })
            emails = []
            for em in (1, 2, 3):
                addr = row.get(f"batchdata_person{p}_email{em}", "").strip()
                if addr:
                    emails.append(addr)
            persons.append({
                "name": name,
                "litigator": row.get(f"batchdata_person{p}_litigator", "") == "true",
                "phones": phones,
                "emails": emails,
            })

        properties.append({
            "owner": row.get("Owner", ""),
            "address": row.get("Address", ""),
            "city": row.get("Municipality", ""),
            "state": row.get("State", ""),
            "zip": row.get("ZIP Code", ""),
            "ownerOccupied": row.get("Owner Occupied", ""),
            "yearBuilt": row.get("dealmachine_year_built", ""),
            "sqft": row.get("dealmachine_living_area_sqft", ""),
            "maxHail": row.get("stormpull_max_hail_size_in", ""),
            "maxHailDate": row.get("stormpull_max_hail_date", ""),
            "lastEventDate": row.get("stormpull_last_event_date", ""),
            "lastEventHail": row.get("stormpull_last_event_hail_size_in", ""),
            "score": row.get("stormpull_exposure_score", ""),
            "batchOwnerName": row.get("batchdata_property_owner_name", ""),
            "persons": persons,
        })
    return properties


def score_sort_key(p):
    # "Severe · 98" -> (tier_rank, value) so Severe sorts above High etc,
    # and higher numeric score sorts first within a tier.
    tier_rank = {"Severe": 0, "High": 1, "Moderate": 2, "Low": 3}
    parts = p["score"].split("·")
    tier = parts[0].strip() if parts else ""
    try:
        value = int(parts[1].strip()) if len(parts) > 1 else 0
    except ValueError:
        value = 0
    return (tier_rank.get(tier, 4), -value)


def main():
    if len(sys.argv) != 3:
        print(__doc__)
        sys.exit(1)
    in_path, out_path = sys.argv[1], sys.argv[2]

    with open(in_path, newline="", encoding="utf-8") as f:
        rows = list(csv.DictReader(f))

    properties = build_properties(rows)
    properties.sort(key=score_sort_key)
    data_json = json.dumps(properties)

    title = html.escape(f"Call Sheet — {len(properties)} Leads")

    html_out = HTML_TEMPLATE.replace("__TITLE__", title).replace("__DATA__", data_json)
    with open(out_path, "w", encoding="utf-8") as f:
        f.write(html_out)
    print(f"wrote {out_path} ({len(properties)} properties)")


HTML_TEMPLATE = r"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>__TITLE__</title>
<style>
  :root {
    --bg: #f7f7f8; --card-bg: #ffffff; --border: #e2e2e6; --text: #1c1c1f;
    --text-dim: #6b6b74; --accent: #2563eb; --accent-bg: #eff4ff;
    --severe: #dc2626; --high: #ea580c; --moderate: #ca8a04; --low: #6b7280;
    --ok: #16a34a; --ok-bg: #f0fdf4; --warn-bg: #fef2f2; --warn: #dc2626;
    --called-bg: #f0fdf4; --section-bg: #fafafa;
    --src-csv: #52525b; --src-csv-bg: #f0f0f2;
    --src-dealmachine: #7c3aed; --src-dealmachine-bg: #f3ebff;
    --src-stormpull: #ea580c; --src-stormpull-bg: #fff1e8;
    --src-batchdata: #2563eb; --src-batchdata-bg: #eff4ff;
    --src-abstract: #16a34a; --src-abstract-bg: #f0fdf4;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #17181c; --card-bg: #202126; --border: #33343a; --text: #e8e8ea;
      --text-dim: #9a9aa2; --accent: #5b8def; --accent-bg: #1b2333;
      --warn-bg: #2a1616; --called-bg: #142218; --ok-bg: #132118; --section-bg: #1b1c21;
      --src-csv: #a1a1aa; --src-csv-bg: #2a2a2e;
      --src-dealmachine: #b794f6; --src-dealmachine-bg: #241a35;
      --src-stormpull: #fb923c; --src-stormpull-bg: #331e10;
      --src-batchdata: #7aa2f7; --src-batchdata-bg: #1b2333;
      --src-abstract: #4ade80; --src-abstract-bg: #132118;
    }
  }
  * { box-sizing: border-box; }
  body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: var(--bg); color: var(--text); }
  header { position: sticky; top: 0; z-index: 10; background: var(--bg);
    border-bottom: 1px solid var(--border); padding: 12px 16px; }
  h1 { font-size: 18px; margin: 0 0 10px; }
  .controls { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
  #search { flex: 1; min-width: 180px; padding: 8px 12px; border-radius: 8px;
    border: 1px solid var(--border); background: var(--card-bg); color: var(--text); font-size: 14px; }
  .toggle { display: flex; align-items: center; gap: 6px; font-size: 13px; color: var(--text-dim);
    padding: 6px 10px; border: 1px solid var(--border); border-radius: 8px; cursor: pointer; user-select: none; }
  .toggle input { cursor: pointer; }
  #progress { font-size: 13px; color: var(--text-dim); margin-left: auto; white-space: nowrap; }
  .legend { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 10px; }
  .legend-label { font-size: 11px; color: var(--text-dim); margin-right: 2px; align-self: center; }
  .src-tag { font-size: 10.5px; font-weight: 700; text-transform: uppercase; letter-spacing: .02em;
    padding: 2px 7px; border-radius: 5px; white-space: nowrap; }
  .src-tag-csv { background: var(--src-csv-bg); color: var(--src-csv); }
  .src-tag-dealmachine { background: var(--src-dealmachine-bg); color: var(--src-dealmachine); }
  .src-tag-stormpull { background: var(--src-stormpull-bg); color: var(--src-stormpull); }
  .src-tag-batchdata { background: var(--src-batchdata-bg); color: var(--src-batchdata); }
  .src-tag-abstract { background: var(--src-abstract-bg); color: var(--src-abstract); }
  .dsection { border-left: 3px solid var(--border); background: var(--section-bg);
    border-radius: 0 6px 6px 0; padding: 7px 10px; margin-top: 8px; }
  .dsection-csv { border-left-color: var(--src-csv); }
  .dsection-dealmachine { border-left-color: var(--src-dealmachine); }
  .dsection-stormpull { border-left-color: var(--src-stormpull); }
  .dsection-batchdata { border-left-color: var(--src-batchdata); }
  .dsection-label { font-size: 10px; font-weight: 700; text-transform: uppercase; letter-spacing: .03em;
    margin-bottom: 4px; }
  .dsection-csv .dsection-label { color: var(--src-csv); }
  .dsection-dealmachine .dsection-label { color: var(--src-dealmachine); }
  .dsection-stormpull .dsection-label { color: var(--src-stormpull); }
  .dsection-batchdata .dsection-label { color: var(--src-batchdata); }
  .dsection-row { display: flex; flex-wrap: wrap; gap: 4px 16px; font-size: 12.5px; color: var(--text-dim); }
  .dsection-row b { color: var(--text); font-weight: 600; }
  main { max-width: 720px; margin: 0 auto; padding: 12px 16px 60px; }
  .card { background: var(--card-bg); border: 1px solid var(--border); border-radius: 12px;
    padding: 14px 16px; margin-bottom: 12px; }
  .card.called { background: var(--called-bg); }
  .card-top { display: flex; justify-content: space-between; align-items: flex-start; gap: 10px; }
  .addr { font-size: 16px; font-weight: 600; }
  .subaddr { font-size: 13px; color: var(--text-dim); }
  .score-badge { font-size: 12px; font-weight: 600; padding: 3px 9px; border-radius: 999px;
    white-space: nowrap; color: #fff; }
  .score-severe { background: var(--severe); }
  .score-high { background: var(--high); }
  .score-moderate { background: var(--moderate); }
  .score-low { background: var(--low); }
  .persons { margin-top: 8px; }
  .person { margin-bottom: 8px; }
  .person:last-child { margin-bottom: 0; }
  .person-name { font-size: 14px; font-weight: 600; }
  .badge { font-size: 10.5px; font-weight: 600; padding: 1px 6px; border-radius: 5px; margin-left: 6px; }
  .badge-litigator { background: var(--warn-bg); color: var(--warn); }
  .phone-row { display: flex; align-items: center; gap: 8px; padding: 4px 0; flex-wrap: wrap; }
  .phone-row a { color: var(--accent); font-weight: 600; text-decoration: none; font-size: 14px; }
  .phone-row a:hover { text-decoration: underline; }
  .phone-row a.phone-done { color: var(--text-dim); text-decoration: line-through; font-weight: 500; }
  .phone-check { display: flex; align-items: center; cursor: pointer; }
  .phone-check input { width: 15px; height: 15px; cursor: pointer; }
  .tag { font-size: 10.5px; padding: 1px 6px; border-radius: 5px; font-weight: 600; }
  .tag-verified-ok { background: var(--src-batchdata-bg); color: var(--src-batchdata); font-weight: 700; }
  .tag-likely-ok { background: var(--src-batchdata-bg); color: var(--src-batchdata); }
  .tag-verified-dead { background: var(--warn-bg); color: var(--warn); font-weight: 700; }
  .tag-likely-dead { background: var(--src-csv-bg); color: var(--src-csv); }
  .tag-dnc { background: var(--warn-bg); color: var(--warn); }
  .tag-type { background: transparent; color: var(--text-dim); font-weight: 500; }
  .tag-abstract-ok { background: var(--ok-bg); color: var(--src-abstract); }
  .tag-abstract-bad { background: var(--warn-bg); color: var(--warn); }
  .email-row a { color: var(--text-dim); font-size: 12.5px; text-decoration: none; }
  .email-row a:hover { text-decoration: underline; }
  .card-actions { margin-top: 12px; padding-top: 10px; border-top: 1px solid var(--border);
    display: flex; align-items: center; gap: 10px; }
  .call-toggle { display: flex; align-items: center; gap: 6px; font-size: 13px; cursor: pointer; }
  .call-toggle input { width: 16px; height: 16px; cursor: pointer; }
  .notes { flex: 1; padding: 6px 10px; font-size: 13px; border: 1px solid var(--border);
    border-radius: 6px; background: var(--card-bg); color: var(--text); }
  .empty-note { color: var(--text-dim); font-size: 13px; font-style: italic; }
  #empty-state { text-align: center; color: var(--text-dim); padding: 40px 0; display: none; }
</style>
</head>
<body>
<header>
  <h1>__TITLE__</h1>
  <div class="controls">
    <input id="search" type="text" placeholder="Search owner, address, city, zip...">
    <label class="toggle"><input type="checkbox" id="onlyReachable"> Only verified-reachable numbers</label>
    <label class="toggle"><input type="checkbox" id="hideCalled"> Hide called</label>
    <span id="progress"></span>
  </div>
  <div class="legend">
    <span class="legend-label">Data sources:</span>
    <span class="src-tag src-tag-csv">CSV</span>
    <span class="src-tag src-tag-dealmachine">DealMachine</span>
    <span class="src-tag src-tag-stormpull">StormPull</span>
    <span class="src-tag src-tag-batchdata">BatchData</span>
    <span class="src-tag src-tag-abstract">Abstract</span>
  </div>
</header>
<main id="list"></main>
<div id="empty-state">No matching properties.</div>

<script>
const DATA = __DATA__;
const STORAGE_KEY = "callsheet_state_v1";
let state = {};
try { state = JSON.parse(localStorage.getItem(STORAGE_KEY) || "{}"); } catch (e) { state = {}; }

function save() { localStorage.setItem(STORAGE_KEY, JSON.stringify(state)); }
function keyFor(p) { return p.address + "|" + p.zip; }

function scoreClass(score) {
  const tier = (score.split("·")[0] || "").trim().toLowerCase();
  return "score-" + (["severe","high","moderate","low"].includes(tier) ? tier : "low");
}

function esc(s) { const d = document.createElement("div"); d.textContent = s || ""; return d.innerHTML; }

// Wraps a group of stats in a colored, labeled block so their source is
// obvious without relying on tiny per-field markers.
function section(src, label, rowHtml) {
  if (!rowHtml) return "";
  return `<div class="dsection dsection-${src}">
    <div class="dsection-label">${label}</div>
    <div class="dsection-row">${rowHtml}</div>
  </div>`;
}

// BatchData's "reachable" flag is a model guess unless "tested" is true —
// only tested+unreachable numbers are confirmed dead. Four distinct
// states, all sourced from BatchData (hence the shared blue/gray
// palette) — confidence (verified vs. modeled) is conveyed by bold
// weight and label text, not a different source color.
function phoneStatusTag(ph) {
  if (ph.tested && ph.reachable) {
    return '<span class="tag tag-verified-ok" title="BatchData live-tested this number and confirmed it is reachable">verified reachable</span>';
  }
  if (ph.tested && !ph.reachable) {
    return '<span class="tag tag-verified-dead" title="BatchData live-tested this number and confirmed it is disconnected">DEAD (verified)</span>';
  }
  if (ph.reachable) {
    return '<span class="tag tag-likely-ok" title="BatchData has not live-tested this number — its model predicts it is reachable">likely reachable (unverified)</span>';
  }
  return '<span class="tag tag-likely-dead" title="BatchData has not live-tested this number — its model predicts it is not reachable">likely unreachable (unverified)</span>';
}

function formatPhone(num) {
  const digits = (num || "").replace(/\D/g, "");
  if (digits.length === 10) {
    return `(${digits.slice(0, 3)}) ${digits.slice(3, 6)}-${digits.slice(6)}`;
  }
  if (digits.length === 11 && digits[0] === "1") {
    return `+1 (${digits.slice(1, 4)}) ${digits.slice(4, 7)}-${digits.slice(7)}`;
  }
  return num;
}

function render() {
  const q = document.getElementById("search").value.trim().toLowerCase();
  const onlyReachable = document.getElementById("onlyReachable").checked;
  const hideCalled = document.getElementById("hideCalled").checked;

  const list = document.getElementById("list");
  list.innerHTML = "";
  let shown = 0;
  let calledCount = 0;

  for (const p of DATA) {
    const k = keyFor(p);
    const s = state[k] || {};
    if (s.called) calledCount++;

    if (hideCalled && s.called) continue;

    if (q) {
      const hay = [p.owner, p.address, p.city, p.zip, p.batchOwnerName,
        ...p.persons.map(pp => pp.name)].join(" ").toLowerCase();
      if (!hay.includes(q)) continue;
    }

    if (onlyReachable) {
      const hasVerifiedReachable = p.persons.some(pp => pp.phones.some(ph => ph.tested && ph.reachable && !ph.dnc));
      if (!hasVerifiedReachable) continue;
    }

    shown++;
    const card = document.createElement("div");
    card.className = "card" + (s.called ? " called" : "");

    let personsHtml = "";
    if (p.persons.length === 0) {
      personsHtml = '<div class="empty-note">No contact found for this property.</div>';
    } else {
      p.persons.forEach((person, personIdx) => {
        let phonesHtml = person.phones.map((ph, phoneIdx) => {
          let abstractTag = "";
          if (ph.abstract) {
            const ok = ph.abstract.valid === true && !ph.abstract.isVoip;
            abstractTag = `<span class="tag ${ok ? 'tag-abstract-ok' : 'tag-abstract-bad'}" title="Validated via Abstract API">
              <span class="src-tag src-tag-abstract" style="margin-right:3px">Abstract</span>${ph.abstract.valid ? "valid" : "invalid"} · ${esc(ph.abstract.lineStatus)} · ${esc(ph.abstract.lineType)}${ph.abstract.isVoip ? " · VOIP" : ""}
            </span>`;
          }
          const phoneCalled = !!(s.phones && s.phones[ph.number]);
          return `
          <div class="phone-row">
            <label class="phone-check" title="Mark this number called">
              <input type="checkbox" class="phone-called-box" data-number="${ph.number}" ${phoneCalled ? "checked" : ""}>
            </label>
            <a href="tel:${ph.number}" data-number="${ph.number}" class="${phoneCalled ? 'phone-done' : ''}">${formatPhone(ph.number)}</a>
            <span class="tag tag-type">${esc(ph.type)}</span>
            ${phoneStatusTag(ph)}
            ${ph.dnc ? '<span class="tag tag-dnc">DO NOT CALL</span>' : ''}
            ${abstractTag}
          </div>`;
        }).join("");
        let emailsHtml = person.emails.map(e => `
          <div class="email-row"><a href="mailto:${e}">${esc(e)}</a></div>`).join("");
        personsHtml += `
          <div class="person">
            <div class="person-name">${esc(person.name)}${person.litigator ? '<span class="badge badge-litigator">litigator</span>' : ''}</div>
            ${phonesHtml || '<div class="empty-note">no phone on file</div>'}
            ${emailsHtml}
          </div>`;
      });
    }

    card.innerHTML = `
      <div class="card-top">
        <div>
          <div class="addr">${esc(p.address)}</div>
          <div class="subaddr">${esc(p.city)}, ${esc(p.state)} ${esc(p.zip)}</div>
        </div>
        ${p.score ? `<span class="score-badge ${scoreClass(p.score)}">${esc(p.score)}</span>` : ''}
      </div>
      ${section('csv', 'Property record — Source CSV', `
        <span>Owner: <b>${esc(p.owner)}</b></span>
        ${p.ownerOccupied ? `<span>Owner-occupied: <b>${esc(p.ownerOccupied)}</b></span>` : ''}
      `)}
      ${section('dealmachine', 'Property details — DealMachine', `
        ${p.yearBuilt ? `<span>Built: <b>${esc(p.yearBuilt)}</b></span>` : ''}
        ${p.sqft ? `<span>Sq ft: <b>${esc(p.sqft)}</b></span>` : ''}
      `.trim())}
      ${section('stormpull', 'Storm damage — StormPull', `
        ${p.maxHail ? `<span>Largest hail: <b>${esc(p.maxHail)}"</b> (${esc(p.maxHailDate)})</span>` : ''}
        ${p.lastEventDate ? `<span>Most recent: <b>${esc(p.lastEventHail)}${p.lastEventHail ? '"' : ''}</b> (${esc(p.lastEventDate)})</span>` : ''}
      `.trim())}
      <div class="dsection dsection-batchdata">
        <div class="dsection-label">Contacts — BatchData</div>
        ${personsHtml}
      </div>
      <div class="card-actions">
        <label class="call-toggle">
          <input type="checkbox" class="called-box" ${s.called ? "checked" : ""}>
          Called
        </label>
        <input type="text" class="notes" placeholder="Notes..." value="${esc(s.notes || '')}">
      </div>
    `;

    card.querySelector(".called-box").addEventListener("change", (e) => {
      state[k] = state[k] || {};
      state[k].called = e.target.checked;
      save();
      card.classList.toggle("called", e.target.checked);
      updateProgress();
    });
    card.querySelector(".notes").addEventListener("input", (e) => {
      state[k] = state[k] || {};
      state[k].notes = e.target.value;
      save();
    });
    card.querySelectorAll(".phone-called-box").forEach(box => {
      box.addEventListener("change", (e) => {
        const num = e.target.dataset.number;
        state[k] = state[k] || {};
        state[k].phones = state[k].phones || {};
        state[k].phones[num] = e.target.checked;
        save();
        const link = card.querySelector(`a[data-number="${num}"]`);
        if (link) link.classList.toggle("phone-done", e.target.checked);
      });
    });

    list.appendChild(card);
  }

  document.getElementById("empty-state").style.display = shown === 0 ? "block" : "none";
  updateProgress();
}

function updateProgress() {
  const total = DATA.length;
  const called = DATA.filter(p => (state[keyFor(p)] || {}).called).length;
  document.getElementById("progress").textContent = `${called} / ${total} called`;
}

document.getElementById("search").addEventListener("input", render);
document.getElementById("onlyReachable").addEventListener("change", render);
document.getElementById("hideCalled").addEventListener("change", render);
render();
</script>
</body>
</html>
"""

if __name__ == "__main__":
    main()
