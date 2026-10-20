// NAS Dashboard — torrents, torrent search, YouTube.
// Classic script: expects shared globals from app.js.

// ==================== ТОРРЕНТЫ ====================
function renderTorrentStats(tasks) {
    let downloading = 0, seeding = 0, finished = 0;
    let dlSpeed = 0, ulSpeed = 0;
    tasks.forEach(t => {
        const s = t.status_num || 1;
        if (s === 2) { downloading++; dlSpeed += (t.speed_download || 0); }
        if (s === 6) { seeding++; ulSpeed += (t.speed_upload || 0); }
        if (s === 5) { finished++; }
    });
    document.getElementById('torrent-stats').innerHTML =
        '<div class="row"><span class="label">📦 Всего торрентов</span><span class="value">' + tasks.length + '</span></div>' +
        '<div class="row"><span class="label">⬇️ Скачивается</span><span class="value">' + downloading + ' (' + formatSpeed(dlSpeed) + ')</span></div>' +
        '<div class="row"><span class="label">🌱 Раздаётся</span><span class="value">' + seeding + ' (' + formatSpeed(ulSpeed) + ')</span></div>' +
        '<div class="row"><span class="label">✅ Завершено</span><span class="value">' + finished + '</span></div>';
}

let allTorrents = [];

function torrentStatusKey(t) {
    const s = t.status_num || 1;
    if (s === 2 || s === 1) return 'downloading';
    if (s === 6) return 'seeding';
    if (s === 3) return 'paused';
    if (s === 5) return 'finished';
    if (s === 7) return 'error';
    return 'downloading';
}

function getFilteredTorrents() {
    const q = ((document.getElementById('torrent-filter-search') || {}).value || '').toLowerCase().trim();
    const st = (document.getElementById('torrent-filter-status') || {}).value || '';
    const sort = (document.getElementById('torrent-sort') || {}).value || 'date';
    let list = allTorrents.slice();
    if (q) list = list.filter(t => (t.title || '').toLowerCase().includes(q));
    if (st) list = list.filter(t => torrentStatusKey(t) === st);
    const ctime = t => ((t.additional && t.additional.detail && t.additional.detail.create_time) || 0);
    switch (sort) {
        case 'name': list.sort((a, b) => (a.title || '').localeCompare(b.title || '')); break;
        case 'size': list.sort((a, b) => (b.size || 0) - (a.size || 0)); break;
        case 'speed': list.sort((a, b) => ((b.speed_download || 0) + (b.speed_upload || 0)) - ((a.speed_download || 0) + (a.speed_upload || 0))); break;
        case 'progress': list.sort((a, b) => (b.progress || 0) - (a.progress || 0)); break;
        default: list.sort((a, b) => ctime(b) - ctime(a));
    }
    return list;
}

async function loadTorrents() {
    try {
        const r = await apiFetch('/api/torrents');
        const data = await r.json();
        let tasks = (data && data.data && data.data.task)
            || (data && data.task)
            || (Array.isArray(data) ? data : []);
        if (!Array.isArray(tasks)) tasks = [];
        allTorrents = tasks;
        console.log('[torrents] loaded:', tasks.length);
        renderTorrentStats(tasks);
        const cnt = document.getElementById('torrent-count');
        if (cnt) cnt.textContent = '(' + tasks.length + ')';
        renderTorrentsPage();
    } catch (e) {
        console.error('loadTorrents failed:', e);
        const el = document.getElementById('torrents');
        if (el) el.innerHTML = '<div class="error">Ошибка загрузки: ' + escapeHtml(e.message) + '</div>';
    }
}

function renderTorrentsPage() {
    const container = document.getElementById('torrents');
    if (!container) return;
    if (allTorrents.length === 0) {
        container.innerHTML = '<div class="card" style="color:var(--fg-dim); text-align:center;">Нет торрентов</div>';
        return;
    }
    const tasks = getFilteredTorrents();
    if (tasks.length === 0) {
        container.innerHTML = '<div class="card" style="color:var(--fg-dim); text-align:center;">Ничего не найдено по фильтру</div>';
        return;
    }
    const activeTasks = tasks.filter(t => t.status_num !== 5 && t.status_num !== 6);
    const completedTasks = tasks.filter(t => t.status_num === 5 || t.status_num === 6);

    let html = '';
    activeTasks.forEach(t => { html += renderTaskCard(t); });

    if (completedTasks.length > 0) {
        html += '<div class="completed-toggle">';
        html += '<button id="toggle-completed-btn" onclick="toggleCompleted()">';
        html += '▸ Показать завершённые (' + completedTasks.length + ')';
        html += '</button></div>';
        html += '<div id="completed-tasks" class="completed-tasks">';
        completedTasks.forEach(t => { html += renderTaskCard(t); });
        html += '</div>';
    }
    container.innerHTML = html;

    // Делегирование вместо inline onclick: id/title из API не попадают в JS.
    container.onclick = (e) => {
        const card = e.target.closest('.torrent[data-torrent-id]');
        if (!card) return;
        const id = Number(card.dataset.torrentId) || 0;
        const btn = e.target.closest('[data-action]');
        if (btn) {
            e.stopPropagation();
            torrentAction(id, btn.dataset.action);
            return;
        }
        openTorrentDetail(id);
    };

    if (completedTasks.length > 0) {
        const expanded = localStorage.getItem('torrentsCompletedExpanded') === 'true';
        const shouldExpand = expanded || activeTasks.length === 0;
        if (!shouldExpand) {
            document.getElementById('completed-tasks').style.display = 'none';
            document.getElementById('toggle-completed-btn').textContent =
                '▸ Показать завершённые (' + completedTasks.length + ')';
        } else {
            document.getElementById('toggle-completed-btn').textContent =
                '▾ Скрыть завершённые (' + completedTasks.length + ')';
        }
    }
}

function fmtEta(seconds) {
    if (!seconds || !isFinite(seconds) || seconds <= 0) return '—';
    const h = Math.floor(seconds / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    const s = Math.floor(seconds % 60);
    if (h > 0) return h + 'ч ' + m + 'м';
    if (m > 0) return m + 'м ' + s + 'с';
    return s + 'с';
}

async function openTorrentDetail(id) {
    const modal = document.getElementById('torrent-detail-modal');
    const t = allTorrents.find(x => String(x.id) === String(id));
    document.getElementById('td-title').textContent = t ? (t.title || 'Без названия') : '…';
    document.getElementById('td-progress').innerHTML = '';
    document.getElementById('td-meta').innerHTML = '<span class="loading" style="padding:0;">Загрузка...</span>';
    document.getElementById('td-trackers').innerHTML = '';
    document.getElementById('td-files').innerHTML = '';
    modal.classList.add('show');
    try {
        const [ir, tr, fr] = await Promise.all([
            fetch('/api/torrent/info/' + encodeURIComponent(id)),
            fetch('/api/torrent/trackers/' + encodeURIComponent(id)),
            fetch('/api/torrent/files/' + encodeURIComponent(id))
        ]);
        const info = await ir.json();
        const trackers = (await tr.json()).trackers || [];
        const files = (await fr.json()).files || [];
        renderTorrentDetail(info, trackers, files);
    } catch (e) {
        document.getElementById('td-meta').innerHTML = '<span style="color:var(--error)">Ошибка: ' + escapeHtml(e.message) + '</span>';
    }
}

function renderTorrentDetail(t, trackers, files) {
    if (!t || t.error) { document.getElementById('td-meta').innerHTML = '<span style="color:var(--error)">Не найдено</span>'; return; }
    const progress = t.progress || 0;
    const statusCls = getStatusClass(t.status_num || 1);
    document.getElementById('td-title').textContent = t.title || 'Без названия';
    document.getElementById('td-progress').innerHTML =
        '<div class="progress-fill ' + statusCls + '" style="width:' + progress.toFixed(1) + '%"></div>' +
        '<div class="progress-text">' + progress.toFixed(1) + '%</div>';

    const size = t.size || 0, downloaded = t.size_downloaded || 0;
    const sd = t.speed_download || 0, su = t.speed_upload || 0;
    const detail = (t.additional && t.additional.detail) || {};
    const remaining = Math.max(size - downloaded, 0);
    const eta = sd > 0 ? remaining / sd : 0;
    const created = detail.create_time ? new Date(detail.create_time * 1000).toLocaleString() : '—';
    let meta = '';
    meta += '<span>Статус: <b>' + getStatusText(t.status_num || 1) + '</b></span>';
    meta += '<span>Размер: <b>' + formatSize(size) + '</b></span>';
    meta += '<span>Скачано: <b>' + formatSize(downloaded) + '</b></span>';
    meta += '<span>⬇️ <b>' + formatSpeed(sd) + '</b></span>';
    meta += '<span>⬆️ <b>' + formatSpeed(su) + '</b></span>';
    meta += '<span>Осталось: <b>' + fmtEta(eta) + '</b></span>';
    meta += '<span>Добавлен: <b>' + created + '</b></span>';
    if (detail.destination) meta += '<span>Назначение: <b>' + escapeHtml(detail.destination) + '</b></span>';
    document.getElementById('td-meta').innerHTML = meta;

    const tb = document.getElementById('td-trackers');
    let th = '';
    (trackers || []).forEach(tr => {
        th += '<div class="td-tracker">';
        th += '<span class="td-url" title="' + escapeHtml(tr.url || '') + '">' + escapeHtml(tr.url || '') + '</span>';
        th += '<span style="flex-shrink:0;color:var(--fg-muted);">🌱 ' + (tr.seeds || 0) + ' · 👥 ' + (tr.peers || 0) + ' · ' + escapeHtml(tr.status || '') + '</span>';
        th += '</div>';
    });
    tb.innerHTML = th || '<div class="loading" style="padding:6px 0;">Трекеров нет</div>';

    const fb = document.getElementById('td-files');
    let fh = '';
    (files || []).forEach(f => {
        fh += '<div class="td-file">';
        fh += '<span class="td-fname" title="' + escapeHtml(f.name || '') + '">' + escapeHtml(f.name || '') + '</span>';
        fh += '<span class="td-fsize">' + formatSize(f.size || 0) + '</span>';
        fh += '</div>';
    });
    fb.innerHTML = fh || '<div class="loading" style="padding:6px 0;">Список файлов пуст</div>';
}

function closeTorrentDetail() {
    document.getElementById('torrent-detail-modal').classList.remove('show');
}

function renderTaskCard(t) {
    const progress = Number(t.progress) || 0;
    const size = Number(t.size) || 0;
    const downloaded = Number(t.size_downloaded) || 0;
    const speedDown = Number(t.speed_download) || 0;
    const statusNum = Number(t.status_num) || 1;
    const statusCls = getStatusClass(statusNum);
    const statusTxt = getStatusText(statusNum);
    const torrentId = Number(t.id) || 0;
    const title = escapeHtml(t.title || 'Без названия');

    let html = '<div class="torrent torrent-clickable" data-torrent-id="' + torrentId + '">';
    html += '<div class="torrent-header">';
    html += '<div class="torrent-title" title="' + title + '">' + title + '</div>';
    html += '<div class="torrent-status status-' + statusCls + '">' + statusTxt + '</div>';
    html += '</div>';
    html += '<div class="progress-wrap">';
    html += '<div class="progress-fill ' + statusCls + '" style="width:' + progress.toFixed(1) + '%"></div>';
    html += '<div class="progress-text">' + progress.toFixed(1) + '%</div>';
    html += '</div>';
    html += '<div class="torrent-meta">';
    html += '<span>📦 ' + formatSize(downloaded) + ' / ' + formatSize(size) + '</span>';
    html += '<span>⬇️ ' + formatSpeed(speedDown) + '</span>';
    html += '<div class="torrent-actions">';
    if (statusNum === 2) {
        html += '<button data-action="pause">⏸</button>';
    } else if (statusNum === 3) {
        html += '<button data-action="resume">▶️</button>';
    }
    html += '<button class="delete" data-action="delete">🗑️</button>';
    html += '</div>';
    html += '</div>';
    html += '</div>';
    return html;
}

function toggleCompleted() {
    const block = document.getElementById('completed-tasks');
    const btn = document.getElementById('toggle-completed-btn');
    if (!block || !btn) return;
    const isHidden = block.style.display === 'none';
    const count = block.querySelectorAll('.torrent').length;
    if (isHidden) {
        block.style.display = '';
        btn.textContent = '▾ Скрыть завершённые (' + count + ')';
        localStorage.setItem('torrentsCompletedExpanded', 'true');
    } else {
        block.style.display = 'none';
        btn.textContent = '▸ Показать завершённые (' + count + ')';
        localStorage.setItem('torrentsCompletedExpanded', 'false');
    }
}

async function torrentAction(id, action) {
    try {
        await fetch('/api/torrents/' + id + '/' + action, { method: 'POST' });
        loadTorrents();
    } catch (e) {
        alert('Ошибка: ' + e.message);
    }
}

async function addTorrent() {
    const magnet = document.getElementById('magnet').value;
    if (!magnet) return;
    const log = document.getElementById('log');
    log.textContent = 'Отправляю...';
    try {
        const r = await fetch('/api/torrents', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({magnet: magnet})
        });
        const d = await r.json();
        log.textContent = JSON.stringify(d);
        document.getElementById('magnet').value = '';
        loadTorrents();
    } catch (e) {
        log.textContent = 'Ошибка: ' + e.message;
    }
}

// ==================== ПОИСК ТОРРЕНТОВ ====================
async function loadIndexers() {
    try {
        const r = await fetch('/api/indexers');
        const d = await r.json();
        const sel = document.getElementById('indexer-filter');
        (d.indexers || []).forEach(i => {
            if (!i.enable) return;
            const opt = document.createElement('option');
            opt.value = i.id;
            opt.textContent = i.name;
            sel.appendChild(opt);
        });
    } catch (e) {
        // молча игнорируем: список индексаторов опционален
    }
}

async function searchTorrentsUI() {
    const query = document.getElementById('torrent-query').value.trim();
    if (!query) return;
    const indexerId = document.getElementById('indexer-filter').value;
    const category = document.getElementById('torrent-category') ? document.getElementById('torrent-category').value : '';
    const res = document.getElementById('search-results');
    res.innerHTML = '<div class="loading">🔍 Ищу торренты...</div>';

    try {
        let url = '/api/search?q=' + encodeURIComponent(query);
        if (indexerId) url += '&indexerIds=' + indexerId;
        if (category) url += '&categories=' + category;
        const r = await fetch(url);
        const d = await r.json();

        if (d.error) {
            res.innerHTML = '<div class="error">Ошибка: ' + escapeHtml(d.error) + '</div>';
            allTorrentResults = [];
            currentPage = 0;
            return;
        }

        allTorrentResults = d.results || [];
        currentPage = 0;
        renderSearchResultsPage();

        const content = document.getElementById('search-results-content');
        const btn = document.getElementById('toggle-search-btn');
        if (content && btn) {
            content.style.display = '';
            btn.textContent = '▾ Свернуть';
            localStorage.setItem('searchResultsCollapsed', 'false');
        }
    } catch (e) {
        res.innerHTML = '<div class="error">Ошибка: ' + escapeHtml(e.message) + '</div>';
    }
}

function renderSearchResultsPage() {
    const res = document.getElementById('search-results');
    const count = document.getElementById('search-count');
    const sort = document.getElementById('search-sort').value;

    const results = [...allTorrentResults];
    results.sort((a, b) => {
        if (sort === 'size') return (b.size || 0) - (a.size || 0);
        if (sort === 'date') return new Date(b.publishDate || 0) - new Date(a.publishDate || 0);
        return (b.seeders || 0) - (a.seeders || 0);
    });

    const total = results.length;
    const totalPages = Math.ceil(total / RESULTS_PER_PAGE) || 1;

    if (total === 0) {
        count.textContent = '🔍 Ничего не найдено';
        res.innerHTML = '';
        return;
    }

    count.textContent = 'Найдено: ' + total + ' торрентов';

    const start = currentPage * RESULTS_PER_PAGE;
    const end = Math.min(start + RESULTS_PER_PAGE, total);
    const pageItems = results.slice(start, end);

    let html = '<div class="search-list">';
    pageItems.forEach(x => {
        const title = escapeHtml(x.title || 'Без названия');
        const magnet = escapeHtml(x.magnet || '');
        const downloadUrl = escapeHtml(x.downloadUrl || '');
        const indexer = escapeHtml(x.indexer || '');
        html += '<div class="search-item">';
        html += '<div class="search-info">';
        html += '<div class="torrent-title" title="' + title + '">' + title + '</div>';
        html += '<div class="search-meta">📦 ' + formatSize(x.size) +
            ' · ⬆️ ' + (Number(x.seeders) || 0) +
            ' · ⬇️ ' + (Number(x.leechers) || 0) +
            ' · ' + indexer + '</div>';
        html += '</div>';
        html += '<button class="dl-btn" data-magnet="' + magnet +
            '" data-url="' + downloadUrl +
            '" data-title="' + title + '">⬇️ Скачать</button>';
        html += '</div>';
    });
    html += '</div>';
    // Слушатель ставим до innerHTML; renderPagination ниже делает innerHTML +=,
    // что не затрагивает обработчик на самом контейнере.
    res.onclick = (e) => {
        const btn = e.target.closest('.dl-btn');
        if (btn) downloadResult(btn.dataset.magnet, btn.dataset.url, btn.dataset.title);
    };
    res.innerHTML = html;

    renderPagination(currentPage, totalPages, total);
}

function renderPagination(current, totalPages, totalItems) {
    const res = document.getElementById('search-results');
    res.innerHTML += '<div class="pagination">' +
        '<button onclick="prevPage()" ' + (current === 0 ? 'disabled' : '') + '>← Назад</button>' +
        '<span class="page-info">Стр. ' + (current + 1) + ' из ' + totalPages + '</span>' +
        '<span class="total-info">(' + totalItems + ' торрентов)</span>' +
        '<button onclick="nextPage()" ' + (current >= totalPages - 1 ? 'disabled' : '') + '>Вперёд →</button>' +
        '</div>';
}

function prevPage() {
    if (currentPage > 0) {
        currentPage--;
        renderSearchResultsPage();
        scrollToResults();
    }
}

function nextPage() {
    const totalPages = Math.ceil(allTorrentResults.length / RESULTS_PER_PAGE);
    if (currentPage < totalPages - 1) {
        currentPage++;
        renderSearchResultsPage();
        scrollToResults();
    }
}

function scrollToResults() {
    const el = document.querySelector('.search-count');
    if (el) el.scrollIntoView({ behavior: 'smooth', block: 'start' });
}

function toggleSearchResults() {
    const content = document.getElementById('search-results-content');
    const btn = document.getElementById('toggle-search-btn');
    if (!content || !btn) return;
    const isHidden = content.style.display === 'none';
    if (isHidden) {
        content.style.display = '';
        btn.textContent = '▾ Свернуть';
        localStorage.setItem('searchResultsCollapsed', 'false');
    } else {
        content.style.display = 'none';
        btn.textContent = '▸ Развернуть';
        localStorage.setItem('searchResultsCollapsed', 'true');
    }
}

async function loadDestinations() {
    try {
        const r = await fetch('/api/destinations');
        const d = await r.json();
        availableDestinations = d.destinations || [];
    } catch (e) {
        availableDestinations = [];
    }
}

function fillDestinationsSelect() {
    const sel = document.getElementById('dest-select');
    if (!sel) return;
    sel.innerHTML = '';
    availableDestinations.forEach(d => {
        const opt = document.createElement('option');
        opt.value = d;
        opt.textContent = d.replace('homes/FuzzyHD/', '📁 ');
        sel.appendChild(opt);
    });
    const saved = localStorage.getItem('selectedDestination');
    if (saved && availableDestinations.includes(saved)) {
        sel.value = saved;
    }
    sel.addEventListener('change', () => {
        localStorage.setItem('selectedDestination', sel.value);
    });
}

async function downloadResult(magnet, downloadUrl, title) {
    if (!magnet && !downloadUrl) { notify('❌ Нет ссылки для скачивания', true); return; }
    notify('📥 Отправлено: ' + title);
    try {
        const body = magnet ? {magnet: magnet} : {torrentUrl: downloadUrl};
        const dest = document.getElementById('dest-select')?.value || '';
        if (dest) body.destination = dest;
        const r = await fetch('/api/torrents', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify(body)
        });
        const d = await r.json();
        if (d.error) {
            notify('❌ Ошибка: ' + d.error, true);
        } else {
            notify('✅ Добавлено: ' + title);
            loadTorrents();
        }
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}

// ==================== YOUTUBE ====================
async function loadYoutubeJobs() {
    try {
        const r = await fetch('/api/youtube/jobs');
        const d = await r.json();
        ytJobs = d.jobs || [];
        renderYoutubeJobs();
        document.getElementById('yt-count').textContent = '(' + ytJobs.length + ')';
    } catch (e) {
        console.error('Не удалось загрузить задачи YouTube:', e);
    }
}

function renderYoutubeJobs() {
    const container = document.getElementById('yt-jobs');
    if (ytJobs.length === 0) {
        container.innerHTML = '<div class="card" style="color:var(--fg-dim); text-align:center;">Нет задач</div>';
        return;
    }
    let html = '';
    ytJobs.forEach(j => {
        const progress = parseInt(j.progress) || 0;
        const statusCls = j.status === 'done' ? 'finished' :
                         j.status === 'error' ? 'error' :
                         j.status === 'cancelled' ? 'paused' : 'downloading';
        const statusTxt = j.status === 'done' ? '✅ Готово' :
                         j.status === 'error' ? '❌ Ошибка' :
                         j.status === 'cancelled' ? '⏸ Отменено' :
                         '⬇️ ' + progress + '%';

        const jid = Number(j.id) || 0;

        html += '<div class="torrent">';
        html += '<div class="torrent-header">';
        html += '<div class="torrent-title">' + escapeHtml(j.title || j.url || '') + '</div>';
        html += '<div class="torrent-status status-' + statusCls + '">' + statusTxt + '</div>';
        html += '</div>';
        html += '<div class="progress-wrap">';
        html += '<div class="progress-fill ' + statusCls + '" style="width:' + progress + '%"></div>';
        html += '<div class="progress-text">' + progress + '%</div>';
        html += '</div>';
        html += '<div class="torrent-meta">';
        html += '<span>' + escapeHtml(j.error || '') + '</span>';
        html += '<div class="torrent-actions">';
        if (j.status === 'done') {
            html += '<button onclick="openYoutubeJob(' + jid + ')">▶️ VLC</button>';
            html += '<button onclick="copyYoutubePath(' + jid + ')">📋 Путь</button>';
        }
        if (j.status === 'downloading' || j.status === 'starting') {
            html += '<button class="delete" onclick="cancelYoutubeJob(' + jid + ')">❌</button>';
        }
        html += '</div></div></div>';
    });
    container.innerHTML = html;
}

async function addYoutubeJob() {
    const url = document.getElementById('yt-url').value.trim();
    const format = document.getElementById('yt-format').value;
    const quality = document.getElementById('yt-quality').value;
    if (!url) return;

    const log = document.getElementById('yt-log');
    log.textContent = 'Отправляю...';
    try {
        const r = await fetch('/api/youtube/download', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({url, format, quality})
        });
        const d = await r.json();
        log.textContent = d.error ? '❌ ' + d.error : '✅ Задача создана: ' + d.job_id;
        if (!d.error) {
            document.getElementById('yt-url').value = '';
            loadYoutubeJobs();
        }
    } catch (e) {
        log.textContent = '❌ ' + e.message;
    }
}

async function cancelYoutubeJob(id) {
    await fetch('/api/youtube/jobs/' + id, {method: 'DELETE'});
    loadYoutubeJobs();
}

async function openYoutubeJob(id) {
    const j = ytJobs.find(x => x.id === id);
    const name = j && j.path ? j.path.split('/').pop() : '';
    if (!name) { alert('Ошибка: путь не найден'); return; }
    try {
        const r = await fetch(NAS_CONFIG.booksOpenerUrl + '/open-video?name=' + encodeURIComponent(name), {
            method: 'POST',
            headers: {'X-Opener': '1'}
        });
        const d = await r.json();
        if (d.error) alert('Ошибка: ' + d.error);
    } catch (e) {
        alert('Ошибка: демон не запущен: ' + e.message);
    }
}

async function copyYoutubePath(id) {
    const j = ytJobs.find(x => x.id === id);
    if (!j || !j.path) { notify('❌ Путь не найден', true); return; }
    const hostPath = NAS_CONFIG.youtubeHostDir + '/' + j.path.split('/').pop();
    try {
        await navigator.clipboard.writeText(hostPath);
        notify('📋 Путь скопирован');
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}
