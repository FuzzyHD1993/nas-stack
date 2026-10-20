// NAS Dashboard — core state, auth, history, settings, tabs.
// Classic script: loaded before feature scripts and shares global scope.

// Runtime config comes from <meta> tags, substituted server-side from env.
const NAS_CONFIG = {
    authEnabled: (document.querySelector('meta[name="nas-auth-enabled"]')?.content || 'false') === 'true',
    booksOpenerUrl: document.querySelector('meta[name="nas-books-opener-url"]')?.content || 'http://localhost:9999',
    youtubeHostDir: document.querySelector('meta[name="nas-youtube-host-dir"]')?.content || '/mnt/nas/YT_video',
};

let allBooks = [];
let allTorrentResults = [];
let currentPage = 0;
const RESULTS_PER_PAGE = 20;
let currentTab = 'today';
let availableDestinations = [];
let ytJobs = [];
let mediaType = 'movie';
let mediaItems = [];
let currentMediaId = null;
let currentMediaTitle = '';
let currentMedia = null;
let mediaScanTimer = null;
let currentSeriesId = null;
let currentSeason = 1;
let currentPlayback = null;
let resumePosition = 0;
let lastProgressSave = 0;

// ==================== v8: STORAGE (localStorage c fallback на sessionStorage) ====================
function storeGet(k) { try { return localStorage.getItem(k); } catch (e) { try { return sessionStorage.getItem(k); } catch (e2) { return null; } } }
function storeSet(k, v) { try { localStorage.setItem(k, v); } catch (e) { try { sessionStorage.setItem(k, v); } catch (e2) {} } }
function storeDel(k) { try { localStorage.removeItem(k); } catch (e) { try { sessionStorage.removeItem(k); } catch (e2) {} } }

// ==================== v8: TOP PROGRESS BAR ====================
let activeReqs = 0;
function ppStart() {
    activeReqs++;
    if (activeReqs !== 1) return;
    const el = document.getElementById('top-progress');
    if (!el) return;
    el.classList.remove('done');
    el.style.width = '15%';
    el.classList.add('active');
    setTimeout(() => { if (activeReqs > 0) el.style.width = '70%'; }, 200);
}
function ppDone() {
    activeReqs = Math.max(0, activeReqs - 1);
    if (activeReqs !== 0) return;
    const el = document.getElementById('top-progress');
    if (!el) return;
    el.style.width = '100%';
    el.classList.add('done');
    setTimeout(() => { el.classList.remove('active', 'done'); el.style.width = '0'; }, 350);
}
function trackRequest(p) {
    let shown = false;
    const t = setTimeout(() => { shown = true; ppStart(); }, 150);
    const done = () => { clearTimeout(t); if (shown) ppDone(); };
    p.then(done, done);
}

// ==================== v8: AUTH ====================
let authToken = storeGet('jwt_token') || null;

// ==================== ADMIN TOKEN (host-exec ручки) ====================
// ADMIN_TOKEN из .env нужен для ручек с побочными эффектами на хосте:
// /api/notify/update/*, /api/notify/env*, /api/scheduler/update/*,
// /api/scheduler/backup/*, /api/stats/cleanup/run, /api/media/open|rename|duplicates.
// Хранится в localStorage и отправляется как 'Authorization: Bearer ...'.
// Отдельный от JWT: apiFetch подставляет jwt_token и перетирал бы админ-токен,
// поэтому админ-вызовы идут через adminFetch (обычный fetch + adminHeaders).
function getAdminToken() {
    let t = storeGet('admin_token');
    if (!t) {
        const entered = window.prompt(
            'Токен администратора (ADMIN_TOKEN из .env).\n' +
            'Нужен для операций на хосте: обновление стека, откат, редактор .env.');
        if (entered) {
            t = entered.trim();
            if (t) storeSet('admin_token', t);
        }
    }
    return t || '';
}

// adminHeaders — заголовки для админ-вызовов (Content-Type не затирается).
function adminHeaders(extra) {
    const h = Object.assign({}, extra || {});
    const t = getAdminToken();
    if (t) h['Authorization'] = 'Bearer ' + t;
    return h;
}

// adminFetch — fetch без подстановки JWT: JWT живёт в том же заголовке.
async function adminFetch(url, options = {}) {
    options.headers = adminHeaders(options.headers);
    const r = await fetch(url, options);
    if (r.status === 401 || r.status === 403) {
        // Токен мог устареть/не совпасть — сбрасываем, чтобы спросить заново.
        storeDel('admin_token');
    }
    return r;
}
// Включена ли авторизация — инжектится сервером из AUTH_ENABLED.
const NAS_AUTH_ENABLED = NAS_CONFIG.authEnabled;

async function apiFetch(url, options = {}) {
    options.headers = options.headers || {};
    if (authToken) options.headers['Authorization'] = 'Bearer ' + authToken;
    const r = await fetch(url, options);
    if (r.status === 401 && NAS_AUTH_ENABLED) { showLogin(); }
    return r;
}

// Глобальная обёртка fetch: добавляет Authorization и прогресс для /api/*
(function () {
    const _origFetch = window.fetch.bind(window);
    window.fetch = function (url, options) {
        options = options || {};
        const isApi = (typeof url === 'string') && url.indexOf('/api/') === 0;
        if (isApi) {
            options.headers = options.headers || {};
            if (authToken && !options.headers['Authorization']) {
                options.headers['Authorization'] = 'Bearer ' + authToken;
            }
        }
        const p = _origFetch(url, options);
        if (isApi) trackRequest(p);
        if (isApi) {
            p.then(r => {
                if (r && r.status === 401 && NAS_AUTH_ENABLED && typeof showLogin === 'function') showLogin();
            }).catch(() => {});
        }
        return p;
    };
})();

async function checkAuth() {
    if (!authToken) { if (NAS_AUTH_ENABLED) showLogin(); else hideLogin(); return !NAS_AUTH_ENABLED; }
    try {
        const r = await fetch('/api/auth/verify', { headers: {'Authorization': 'Bearer ' + authToken} });
        const d = await r.json().catch(() => ({}));
        if (!d.valid) { storeDel('jwt_token'); authToken = null; if (NAS_AUTH_ENABLED) showLogin(); else hideLogin(); return !NAS_AUTH_ENABLED; }
        hideLogin();
        return true;
    } catch (e) {
        if (NAS_AUTH_ENABLED) { showLogin(); return false; }
        hideLogin();
        return true;
    }
}

function showLogin() {
    if (!NAS_AUTH_ENABLED) return; // при AUTH_ENABLED=false логин не нужен
    const el = document.getElementById('login-screen');
    if (el) el.style.display = 'flex';
}

function hideLogin() {
    const el = document.getElementById('login-screen');
    if (el) el.style.display = 'none';
}

async function doLogin() {
    const pass = document.getElementById('login-password').value;
    if (!pass) return;
    const err = document.getElementById('login-error');
    err.textContent = '';
    try {
        const r = await fetch('/api/auth/login', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({password: pass})
        });
        const d = await r.json().catch(() => ({}));
        if (d.token) {
            authToken = d.token;
            storeSet('jwt_token', d.token);
            hideLogin();
            location.reload();
        } else {
            err.textContent = d.error || 'Неверный пароль';
        }
    } catch (e) {
        err.textContent = 'Ошибка: ' + e.message;
    }
}

function doLogout() {
    storeDel('jwt_token');
    authToken = null;
    location.reload();
}

// ==================== v8: COMMAND PALETTE ====================
const COMMANDS = [
    {id: 'tab-stats', label: '📊 Метрики', action: () => showTab('stats')},
    {id: 'smart-search', label: '✨ Умный поиск фильмов', action: () => openSmartSearch()},
    {id: 'duplicates', label: '🔁 Дубликаты в библиотеке', action: () => openDuplicates()},
    {id: 'tab-torrents', label: '📋 Торренты', action: () => showTab('torrents')},
    {id: 'tab-books', label: '📚 Книги', action: () => showTab('books')},
    {id: 'tab-music', label: '🎵 Музыка', action: () => showTab('music')},
    {id: 'tab-youtube', label: '📹 YouTube', action: () => showTab('youtube')},
    {id: 'tab-media', label: '🎬 Медиа', action: () => showTab('media')},
    {id: 'tab-photos', label: '🖼️ Фото', action: () => showTab('photos')},
    {id: 'tab-history', label: '📜 История', action: () => showTab('history')},
    {id: 'tab-settings', label: '⚙️ Настройки', action: () => showTab('settings')},
    {id: 'theme-mocha', label: '🎨 Тема: Catppuccin Mocha', action: () => changeTheme('mocha')},
    {id: 'theme-latte', label: '☀️ Тема: Catppuccin Latte', action: () => changeTheme('latte')},
    {id: 'theme-dracula', label: '🧛 Тема: Dracula', action: () => changeTheme('dracula')},
    {id: 'theme-nord', label: '❄️ Тема: Nord', action: () => changeTheme('nord')},
    {id: 'theme-tokyo-night', label: '🌃 Тема: Tokyo Night', action: () => changeTheme('tokyo-night')},
    {id: 'theme-rose-pine', label: '🌹 Тема: Rosé Pine', action: () => changeTheme('rose-pine')},
    {id: 'theme-everforest', label: '🌲 Тема: Everforest', action: () => changeTheme('everforest')},
    {id: 'theme-kanagawa', label: '🌊 Тема: Kanagawa', action: () => changeTheme('kanagawa')},
    {id: 'theme-gruvbox', label: '🟫 Тема: Gruvbox', action: () => changeTheme('gruvbox')},
    {id: 'theme-one-dark', label: '⚫ Тема: One Dark', action: () => changeTheme('one-dark')},
    {id: 'theme-auto', label: '🌗 Тема: Авто', action: () => changeTheme('auto')},
    {id: 'scan-media', label: '🔄 Сканировать медиа', action: () => { showTab('media'); const b = document.querySelector('.media-controls button'); if (b) scanMedia({target: b}); }},
    {id: 'search-media', label: '🔍 Глобальный поиск', action: openGlobalSearch},
    {id: 'sync-credits', label: '🎭 Загрузить актёров', action: () => { adminFetch('/api/media/scan/credits', {method: 'POST'}).then(() => notify('🎭 Загрузка актёров запущена')); }},
    {id: 'export-settings', label: '📥 Экспорт настроек', action: exportSettings},
    {id: 'logout', label: '🔓 Выйти', action: doLogout},
];

let cmdFiltered = [];
let cmdActive = -1;

function openCommandPalette() {
    const el = document.getElementById('command-palette');
    if (!el) return;
    el.classList.add('show');
    const inp = document.getElementById('cmd-input');
    inp.value = '';
    renderCommands('');
    setTimeout(() => inp.focus(), 30);
}

function closeCommandPalette() {
    const el = document.getElementById('command-palette');
    if (el) el.classList.remove('show');
}

function renderCommands(query) {
    const q = (query || '').toLowerCase();
    cmdFiltered = COMMANDS.filter(c => c.label.toLowerCase().includes(q));
    cmdActive = cmdFiltered.length ? 0 : -1;
    let html = '';
    cmdFiltered.forEach((c, i) => {
        html += '<div class="cmd-item' + (i === cmdActive ? ' active' : '') + '" data-idx="' + i + '" onclick="runCommand(\'' + c.id + '\')">' + c.label + '</div>';
    });
    document.getElementById('cmd-list').innerHTML = html || '<div class="cmd-item" style="color:var(--fg-dim);">Ничего не найдено</div>';
}

function runCommand(id) {
    const cmd = COMMANDS.find(c => c.id === id);
    if (!cmd) return;
    closeCommandPalette();
    try { cmd.action(); } catch (e) { notify('❌ ' + e.message, true); }
}

function cmdKeydown(e) {
    if (e.key === 'Escape') { e.preventDefault(); closeCommandPalette(); return; }
    if (e.key === 'Enter') {
        e.preventDefault();
        if (cmdActive >= 0 && cmdFiltered[cmdActive]) runCommand(cmdFiltered[cmdActive].id);
        return;
    }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        if (!cmdFiltered.length) return;
        cmdActive = (cmdActive + (e.key === 'ArrowDown' ? 1 : -1) + cmdFiltered.length) % cmdFiltered.length;
        document.querySelectorAll('#cmd-list .cmd-item').forEach((el, i) => {
            el.classList.toggle('active', i === cmdActive);
            if (i === cmdActive) el.scrollIntoView({block: 'nearest'});
        });
    }
}

function formatTime(sec) {
    if (!sec || sec < 0) return '0:00';
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = Math.floor(sec % 60);
    return h > 0 ? h + ':' + String(m).padStart(2, '0') + ':' + String(s).padStart(2, '0') : m + ':' + String(s).padStart(2, '0');
}

function showTab(name) {
    currentTab = name;
    document.querySelectorAll('.tab').forEach(t => t.classList.toggle('active', t.dataset.tab === name));
    document.querySelectorAll('.tab-content').forEach(c => c.classList.toggle('active', c.id === 'tab-' + name));

    if (name === 'history') { loadHistory(); loadDownloadStats(); }
    if (name === 'today') loadToday();
    if (name === 'books') loadDownloaded();
    if (name === 'media') { loadMedia(); loadContinueWatching(); loadMediaFilters(); loadLibraryStats(); loadSmartShelves(); resetNewMediaBadge(); }
    if (name === 'music') { musicLoadStatus(); loadAudioMode().then(musicLoadAlbums); loadMusicSidebar(); }
    if (name === 'torrents') { loadRutrackerProfile(); loadTorrentStats(); loadTorrents(); loadDsSettings(); }
    if (name === 'photos') loadPhotos();
    if (name === 'cinema') { loadCinemaContent(); resetCinemaBadge(); }
    if (name === 'health') loadHealth();
    if (name === 'settings') populateSettings();
}

// ==================== ✨ УМНЫЙ ПОИСК ====================
function openSmartSearch() {
    document.getElementById('smart-search-modal').classList.add('show');
    document.getElementById('smart-search-input').focus();
}
async function runSmartSearch() {
    const q = document.getElementById('smart-search-input').value.trim();
    const paramsEl = document.getElementById('smart-search-params');
    const resEl = document.getElementById('smart-search-results');
    resEl.innerHTML = '<div class="loading">Поиск...</div>';
    try {
        const r = await apiFetch('/api/search/smart', {
            method: 'POST', headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({query: q})
        });
        const d = await r.json();
        const movies = d.movies || d.items || [];
        paramsEl.textContent = q ? ('Запрос: «' + q + '»') : '';
        if (!movies.length) { resEl.innerHTML = '<div class="loading">Ничего не найдено</div>'; return; }
        let html = '<div class="music-albums-grid">';
        movies.forEach(m => {
            const poster = m.poster_url ? ('/api/media/poster?url=' + encodeURIComponent(m.poster_url)) : '';
            html += '<div class="album-card" onclick="openMediaModal(' + m.id + ')">' +
                (poster ? '<img src="' + poster + '" loading="lazy">' : '') +
                '<div class="album-card-title">' + escapeHtml(m.title || '') + (m.year ? ' (' + m.year + ')' : '') + '</div>' +
                '<div style="font-size:11px;color:var(--fg-dim);">★ ' + (m.rating || 0).toFixed(1) + (m.runtime ? ' · ' + m.runtime + ' мин' : '') + '</div></div>';
        });
        html += '</div>';
        resEl.innerHTML = html;
    } catch (e) { resEl.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}

// ==================== 🔁 ДУБЛИКАТЫ ====================
async function openDuplicates() {
    document.getElementById('duplicates-modal').classList.add('show');
    const el = document.getElementById('duplicates-list');
    el.innerHTML = '<div class="loading">Загрузка...</div>';
    try {
        const r = await apiFetch('/api/media/duplicates');
        const d = await r.json();
        const dups = d.duplicates || [];
        if (!dups.length) { el.innerHTML = 'Дубликатов нет ✅'; return; }
        let html = '';
        dups.forEach(dup => {
            html += '<div style="border:1px solid var(--glass-border);border-radius:8px;padding:10px;margin-bottom:10px;">';
            html += '<div style="font-weight:600;margin-bottom:6px;">' + escapeHtml(dup.title || '') + ' (' + dup.versions.length + ' версии)</div>';
            dup.versions.forEach(v => {
                html += '<label style="display:flex;align-items:center;gap:8px;font-size:12px;padding:3px 0;">' +
                    '<input type="radio" name="keep-' + dup.tmdb_id + '" value="' + v.id + '">' +
                    '<span>' + escapeHtml(v.resolution) + ' | ' + fmtSpeed(v.size_bytes) + ' | #' + v.id + '</span>' +
                    '<span style="color:var(--fg-dim);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:320px;" title="' + escapeHtml(v.path) + '">' + escapeHtml(v.path) + '</span></label>';
            });
            html += '<button onclick="resolveDup(' + dup.tmdb_id + ')" style="margin-top:6px;">Оставить выбранное, убрать остальные</button></div>';
        });
        el.innerHTML = html;
    } catch (e) { el.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}
async function resolveDup(tmdbId) {
    const rad = document.querySelector('input[name="keep-' + tmdbId + '"]:checked');
    if (!rad) { notify('Выберите, какую версию оставить', true); return; }
    const keepId = parseInt(rad.value);
    const nDel = document.querySelectorAll('input[name="keep-' + tmdbId + '"]').length - 1;
    if (nDel > 0 && !confirm('Удалить ' + nDel + ' файл(ов) для TMDB ID ' + tmdbId + '? Файлы переносятся в /mnt/nas/Backups/duplicates. Действие необратимо.')) return;

    // Список мог устареть: между отрисовкой окна и кликом файлы переименовали/
    // удалили. Поэтому берём свежий список и ВАЛИДИРУЕМ keepId по нему —
    // иначе можно было бы отправить keep_id, которого уже нет (или который
    // принадлежит другой группе) и удалить не те файлы.
    let d0;
    try {
        const r0 = await apiFetch('/api/media/duplicates');
        d0 = await r0.json();
    } catch (e) {
        notify('❌ Не удалось обновить список дублей: ' + e.message, true);
        return;
    }
    const dup = (d0.duplicates || []).find(x => Number(x.tmdb_id) === Number(tmdbId));
    if (!dup) {
        notify('Дубликаты для TMDB ID ' + tmdbId + ' больше не найдены (уже обработаны)', true);
        openDuplicates();
        return;
    }
    const freshIds = dup.versions.map(v => Number(v.id));
    if (!freshIds.includes(keepId)) {
        notify('Выбранная версия #' + keepId + ' уже отсутствует в списке — обновите окно', true);
        openDuplicates();
        return;
    }
    const deleteIds = dup.versions.map(v => v.id).filter(id => Number(id) !== keepId);
    if (deleteIds.length === 0) {
        notify('Нечего удалять: осталась одна версия', true);
        openDuplicates();
        return;
    }
    try {
        await adminFetch('/api/media/duplicates/resolve', {
            method: 'POST', headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({tmdb_id: tmdbId, keep_id: keepId, delete_ids: deleteIds})
        });
        notify('✅ Версии убраны (файлы перенесены в /mnt/nas/Backups/duplicates)');
        openDuplicates();
    } catch (e) { notify('❌ ' + e.message, true); }
}

// ==================== 📱 PWA МОБИЛЬНЫЙ ПУЛЬТ (?pwa=music) ====================
function initPwaMusic() {
    const params = new URLSearchParams(location.search);
    if (params.get('pwa') !== 'music') return;
    document.body.classList.add('pwa-music');
    const upd = async () => {
        try {
            const r = await fetch('/api/music/status');
            const d = await r.json();
            const si = d.song_info || {};
            const art = si.Artist || si.artist || '';
            const alb = si.Album || si.album || '';
            const tit = si.Title || si.title || (si.file || '').split('/').pop();
            document.getElementById('pwa-title').textContent = tit || '—';
            document.getElementById('pwa-artist').textContent = art || '—';
            const cover = document.getElementById('pwa-cover');
            if (art && alb) { cover.src = '/api/music/covers/' + encodeURIComponent(art) + '/' + encodeURIComponent(alb) + '?v=v6'; cover.style.display = ''; }
            const vol = document.getElementById('pwa-volume');
            if (d.volume !== undefined && vol) vol.value = d.volume;
        } catch (e) {}
    };
    upd();
    setInterval(upd, 5000);
}
initPwaMusic();

// ==================== 🏠 СЕГОДНЯ ====================
function fmtSpeed(bps) {
    if (!bps || bps <= 0) return '0 Б/с';
    const u = ['Б/с','КБ/с','МБ/с','ГБ/с'];
    let i = 0, v = bps;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(1) + ' ' + u[i];
}
// cleanMetaName убирает мусор из тегов для отображения.
function cleanMetaName(v) {
    if (!v) return '';
    return v.replace(/\s*[\(\[\{][^\)\]\}]*[\)\]\}]/g, '')
        .replace(/\b(19|20)\d{2}\b/g, '')
        .replace(/\b(cd\d*|disc\d*|remaster(ed)?|deluxe|anniversary|expanded|bonus|live|ep|single)\b/gi, '')
        .replace(/\s+/g, ' ').trim();
}
async function loadToday() {
    // Торренты
    (async () => {
        const el = document.getElementById('today-torrents');
        try {
            const r = await apiFetch('/api/torrent/stats');
            const d = await r.json();
            el.innerHTML = '<h3>📥 Торренты</h3><div>Активных: <b>' + (d.active || d.total || 0) + '</b></div>' +
                '<div style="color:var(--fg-muted);font-size:13px;">↓ ' + fmtSpeed(d.dl_speed) + ' · ↑ ' + fmtSpeed(d.ul_speed) + '</div>';
        } catch (e) { el.innerHTML = '<h3>📥 Торренты</h3><div class="loading">Нет данных</div>'; }
    })();
    // Музыка
    (async () => {
        const el = document.getElementById('today-music');
        try {
            const r = await apiFetch('/api/music/status');
            const d = await r.json();
            const si = d.song_info || {};
            if (d.state === 'play' || d.state === 'pause') {
                const art = si.Artist || si.artist || '';
                const tit = si.Title || si.title || si.file || '';
                el.innerHTML = '<h3>🎵 ' + (d.state === 'play' ? 'Сейчас играет' : 'Пауза') + '</h3>' +
                    '<div>' + escapeHtml(art) + (art ? ' — ' : '') + escapeHtml(tit) + '</div>' +
                    '<div style="color:var(--fg-muted);font-size:12px;">' + fmtMusicTime(d.elapsed || 0) + ' / ' + fmtMusicTime(d.duration || 0) + '</div>';
            } else {
                el.innerHTML = '<h3>🎵 Музыка</h3><div class="loading">Плеер остановлен</div>';
            }
        } catch (e) { el.innerHTML = '<h3>🎵 Музыка</h3><div class="loading">Нет данных</div>'; }
    })();
    // NAS / диски
    (async () => {
        const el = document.getElementById('today-nas');
        try {
            const r = await apiFetch('/api/stats/disks');
            const d = await r.json();
            const arr = Array.isArray(d) ? d : (d.disks || []);
            const root = arr.find(x => x.mount === '/' || x.mount === '/mnt/nas') || arr[0];
            if (root) {
                el.innerHTML = '<h3>💾 Диски</h3><div>' + escapeHtml(root.mount || '/') + ': <b>' + (root.percent || root.used_percent || '?') + '%</b></div>' +
                    '<div style="color:var(--fg-muted);font-size:12px;">' + (root.used_human || '') + ' / ' + (root.total_human || '') + '</div>';
            } else { el.innerHTML = '<h3>💾 Диски</h3><div class="loading">Нет данных</div>'; }
        } catch (e) { el.innerHTML = '<h3>💾 Диски</h3><div class="loading">Нет данных</div>'; }
    })();
    // YouTube
    (async () => {
        const el = document.getElementById('today-youtube');
        try {
            const r = await apiFetch('/api/youtube/jobs');
            const d = await r.json();
            const jobs = d.jobs || [];
            const today = new Date().toDateString();
            const recent = jobs.filter(j => j.created_at && new Date(j.created_at).toDateString() === today);
            el.innerHTML = '<h3>📹 YouTube</h3><div>Сегодня: <b>' + recent.length + '</b></div>' +
                '<div style="color:var(--fg-muted);font-size:12px;">Всего: ' + jobs.length + '</div>';
        } catch (e) { el.innerHTML = '<h3>📹 YouTube</h3><div class="loading">Нет данных</div>'; }
    })();
    // Здоровье сервисов
    (async () => {
        const el = document.getElementById('today-health');
        try {
            const r = await apiFetch('/api/scheduler/health/summary');
            const d = await r.json();
            el.innerHTML = '<h3>🩺 Сервисы</h3><div>UP: <b style="color:var(--success,#a6e3a1);">' + (d.up || 0) + '</b> · DOWN: <b style="color:var(--error,#f38ba8);">' + (d.down || 0) + '</b></div>' +
                '<div style="color:var(--fg-muted);font-size:12px;">Всего: ' + (d.total || 0) + '</div>';
        } catch (e) { el.innerHTML = '<h3>🩺 Сервисы</h3><div class="loading">Нет данных</div>'; }
    })();
}

function notify(msg, isError) {
    const n = document.getElementById('notification');
    n.textContent = msg;
    n.className = 'notify show' + (isError ? ' error' : '');
    setTimeout(() => { n.className = 'notify' + (isError ? ' error' : ''); }, 3000);
}

function formatSize(bytes) {
    if (!bytes || bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return (bytes / Math.pow(k, i)).toFixed(2) + ' ' + sizes[i];
}

function formatSpeed(bps) {
    if (!bps || bps === 0) return '—';
    return formatSize(bps) + '/s';
}

function skeletonGrid(n) {
    n = n || 12;
    let html = '';
    for (let i = 0; i < n; i++) html += '<div class="skeleton-poster"></div>';
    return html;
}

function getStatusClass(s) {
    return {1:'waiting',2:'downloading',3:'paused',5:'finished',6:'seeding',7:'error'}[s] || 'waiting';
}

function getStatusText(s) {
    return {1:'⏸ Ожидание',2:'⬇️ Скачивание',3:'⏸ Пауза',5:'✅ Завершён',6:'🌱 Раздача',7:'❌ Ошибка'}[s] || 'Неизвестно';
}

// ==================== МЕТРИКИ ====================
function metricBar(label, pct, text) {
    const color = pct < 60 ? 'finished' : (pct < 85 ? 'paused' : 'error');
    return '<div class="row"><span class="label">' + label + '</span><span class="value">' + text + '</span></div>' +
        '<div class="progress-wrap" style="height:14px;margin-bottom:12px;">' +
        '<div class="progress-fill ' + color + '" style="width:' + Math.min(pct, 100).toFixed(1) + '%"></div>' +
        '</div>';
}

async function loadStats() {
    try {
        const r = await fetch('/api/stats');
        const d = await r.json();
        let html = '';
        if (d.cpu) {
            const cpu = parseFloat(d.cpu.usage) || 0;
            html += metricBar('CPU', cpu, cpu.toFixed(1) + '%');
        }
        if (d.ram) {
            const pct = parseFloat(d.ram.percent) || 0;
            html += metricBar('RAM', pct, d.ram.used_gb + ' / ' + d.ram.total_gb + ' GB (' + pct.toFixed(1) + '%)');
        }
        if (d.disks && d.disks.length) {
            d.disks.forEach(disk => {
                const pct = parseFloat(disk.percent) || 0;
                html += metricBar('💾 ' + disk.mount, pct, disk.used + ' / ' + disk.total + ' (' + pct.toFixed(0) + '%)');
            });
        }
        if (d.gpu && !d.gpu.error) {
            html += '<div class="row"><span class="label">🎮 GPU</span><span class="value">' + d.gpu.temp + ' | ' + d.gpu.usage + '</span></div>';
        }
        document.getElementById('stats').innerHTML = html;
    } catch (e) {
        document.getElementById('stats').innerHTML = 'Ошибка: ' + e.message;
    }
}

async function loadExtendedStats() {
    try {
        const [loadR, coresR, netR, procR] = await Promise.all([
            fetch('/api/stats/load'), fetch('/api/stats/cpu/cores'), fetch('/api/stats/network'), fetch('/api/stats/processes?limit=10')
        ]);
        const load = await loadR.json();
        const cores = await coresR.json();
        const net = await netR.json();
        const procs = await procR.json();

        // Load / Uptime
        let days = Math.floor((load.uptime_sec || 0) / 86400);
        let hours = Math.floor(((load.uptime_sec || 0) % 86400) / 3600);
        let mins = Math.floor(((load.uptime_sec || 0) % 3600) / 60);
        document.getElementById('load-info').innerHTML =
            '<div class="row"><span class="label">Load average</span><span class="value">' + (load.load1 || 0) + ' / ' + (load.load5 || 0) + ' / ' + (load.load15 || 0) + '</span></div>' +
            '<div class="row"><span class="label">Uptime</span><span class="value">' + days + 'д ' + hours + 'ч ' + mins + 'м</span></div>';

        // CPU cores
        let coresHtml = '<div style="display:grid; grid-template-columns:repeat(auto-fill,minmax(140px,1fr)); gap:8px;">';
        (cores || []).forEach(c => {
            const u = parseFloat(c.usage) || 0;
            coresHtml += '<div><div style="font-size:11px;color:var(--fg-muted);">' + c.id + '</div>' +
                '<div class="progress-wrap" style="height:10px;"><div class="progress-fill downloading" style="width:' + Math.min(100, u) + '%"></div></div>' +
                '<div style="font-size:10px;color:var(--fg-dim);">' + c.usage + '%</div></div>';
        });
        coresHtml += '</div>';
        document.getElementById('cpu-cores').innerHTML = coresHtml;

        // Network
        let netHtml = '';
        (net || []).forEach(n => {
            netHtml += '<div class="row"><span class="label">' + n.iface + '</span><span class="value">↓ ' + formatSize(n.rx_bytes) + ' · ↑ ' + formatSize(n.tx_bytes) + '</span></div>';
        });
        document.getElementById('network-info').innerHTML = netHtml || '<div style="color:var(--fg-dim);">Нет данных</div>';

        // NAS
        try {
            const nasR = await fetch('/api/stats/nas');
            const nas = await nasR.json();
            let nasHtml = '';
            (nas.dirs || []).forEach(d => {
                nasHtml += '<div class="row"><span class="label">' + d.path + '</span><span class="value">' + d.size + '</span></div>';
            });
            document.getElementById('nas-info').innerHTML = nasHtml || '<div style="color:var(--fg-dim);">Нет данных</div>';
        } catch (e) {}

        // Processes
        let procHtml = '<div class="history-list">';
        (procs || []).forEach(p => {
            procHtml += '<div class="history-item"><span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">' + escapeHtml(p.name) + '</span><span class="time">CPU ' + p.cpu + '% · MEM ' + p.mem + '%</span></div>';
        });
        procHtml += '</div>';
        document.getElementById('processes-info').innerHTML = procHtml || '<div style="color:var(--fg-dim);">Нет данных</div>';

        // Температуры + SMART (не чаще раза в 30с — smartctl тяжёлый)
        if (!window.__lastSensorFetch || Date.now() - window.__lastSensorFetch > 30000) {
            window.__lastSensorFetch = Date.now();
            loadSensorsAndSmart();
        }
    } catch (e) {
        // молча — секции останутся с ошибкой
    }
}

async function loadSensorsAndSmart() {
    // Температуры
    try {
        const sr = await fetch('/api/stats/sensors');
        const sensors = await sr.json();
        let html = '';
        const cp = sensors.cpu_package || {};
        if (cp.temp) {
            const t = cp.temp;
            const col = t < 50 ? 'success' : (t < 70 ? 'warning' : 'error');
            html += '<div class="row"><span class="label">CPU Package</span><span style="color:var(--' + col + ')">' + t.toFixed(1) + '°C</span></div>';
        }
        (sensors.cores || []).slice(0, 8).forEach(c => {
            html += '<div class="row"><span class="label">' + escapeHtml(c.label) + '</span><span>' + c.temp.toFixed(1) + '°C</span></div>';
        });
        (sensors.nvme || []).forEach(n => {
            html += '<div class="row"><span class="label">NVMe ' + escapeHtml(n.label) + '</span><span>' + n.temp.toFixed(1) + '°C</span></div>';
        });
        document.getElementById('sensors-info').innerHTML = html || '<div style="color:var(--fg-dim);">Нет данных</div>';
    } catch (e) {}

    // SMART
    try {
        const smr = await fetch('/api/stats/smart');
        const smart = await smr.json();
        let html = '<div class="history-list">';
        (smart || []).forEach(d => {
            if (d.error) {
                html += '<div class="history-item"><span style="flex:1">' + escapeHtml(d.device) + '</span><span style="color:var(--error)">' + escapeHtml(d.error) + '</span></div>';
                return;
            }
            const ok = d.health === 'PASSED';
            html += '<div class="history-item">' +
                '<span style="flex:1">' + escapeHtml(d.device) + ' — ' + escapeHtml(d.model || '?') + '</span>' +
                '<span style="color:var(--' + (ok ? 'success' : 'error') + ')">' +
                (ok ? '✅' : '❌') + ' ' + d.health +
                (d.temperature ? ' · ' + d.temperature + '°C' : '') +
                (d.wear_level !== undefined ? ' · wear ' + d.wear_level + '%' : '') +
                '</span></div>';
        });
        html += '</div>';
        document.getElementById('smart-info').innerHTML = html;
    } catch (e) {}
}

async function loadStatus() {
    try {
        const r = await fetch('/api/status');
        const d = await r.json();
        let html = '';
        for (const [name, status] of Object.entries(d)) {
            const ok = status.includes('"status":"ok"');
            html += '<div class="row"><span class="label">' + name + '</span><span style="color:' + (ok?'var(--success)':'var(--error)') + '">' + (ok ? '✅ ok' : '❌ down') + '</span></div>';
        }
        document.getElementById('status').innerHTML = html;
    } catch (e) {
        document.getElementById('status').innerHTML = 'Ошибка: ' + e.message;
    }
}


// ==================== ИСТОРИЯ ====================
function getEventColor(type) {
    return {
        'torrent.added': 'var(--accent)',
        'torrent.completed': 'var(--success)',
        'disk.warning': 'var(--warning)'
    }[type] || 'var(--fg)';
}

function getEventIcon(type) {
    return {
        'torrent.added': '📥',
        'torrent.completed': '✅',
        'disk.warning': '⚠️'
    }[type] || '📌';
}

function isSameDay(a, b) {
    return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

function getDateGroup(dateStr) {
    const d = new Date(dateStr);
    const today = new Date();
    const yesterday = new Date();
    yesterday.setDate(today.getDate() - 1);
    if (isSameDay(d, today)) return 'Сегодня';
    if (isSameDay(d, yesterday)) return 'Вчера';
    return d.toLocaleDateString('ru-RU');
}

function escapeHtml(s) {
    return String(s == null ? '' : s)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
}

function prettyPayload(type, payload) {
    if (!payload) return '';
    if (payload.startsWith('magnet:')) {
        const match = payload.match(/.*[?&]dn=([^&]+)/);
        if (match) {
            let name;
            try {
                name = decodeURIComponent(match[1].replace(/\+/g, ' '));
            } catch (e) {
                name = match[1].replace(/\+/g, ' ');
            }
            return type === 'torrent.added' ? '📥 ' + name :
                   type === 'torrent.completed' ? '✅ ' + name : '📦 ' + name;
        }
        const hashMatch = payload.match(/btih:([a-fA-F0-9]{8})/);
        if (hashMatch) return '📦 <magnet ' + hashMatch[1] + '…>';
    }
    return payload.length > 120 ? payload.substring(0, 120) + '…' : payload;
}

async function loadHistory() {
    try {
        const r = await fetch('/api/books/history');
        const d = await r.json();
        const events = d.events || [];

        if (events.length === 0) {
            document.getElementById('history').innerHTML = '<div class="loading">История пуста</div>';
            return;
        }

        let html = '';
        let lastGroup = '';
        events.forEach(e => {
            const group = getDateGroup(e.created_at);
            if (group !== lastGroup) {
                html += '<div class="history-group">' + escapeHtml(group) + '</div>';
                lastGroup = group;
            }
            const date = new Date(e.created_at).toLocaleString('ru-RU');
            const payload = e.payload || '';
            const color = getEventColor(e.type);
            html += '<div class="history-item" style="border-left:3px solid ' + color + ';">';
            html += '<span class="history-icon">' + getEventIcon(e.type) + '</span>';
            html += '<div style="flex:1;min-width:0;">';
            html += '<div style="color:' + color + ';"><strong>' + escapeHtml(e.type) + '</strong></div>';
            html += '<div class="history-payload" title="' + escapeHtml(payload) + '">' + escapeHtml(prettyPayload(e.type, payload)) + '</div>';
            html += '</div>';
            html += '<div class="time">' + date + '</div>';
            html += '</div>';
        });
        document.getElementById('history').innerHTML = html;
    } catch (e) {
        document.getElementById('history').innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

// ==================== НАСТРОЙКИ (ТЕМА) ====================
const THEMES = ['mocha', 'latte', 'dracula', 'nord', 'tokyo-night', 'rose-pine', 'everforest', 'kanagawa', 'gruvbox', 'one-dark', 'auto'];

const THEME_LABELS = {
    'auto': '🌗 Авто', 'mocha': '🌙 Catppuccin Mocha', 'latte': '☀️ Catppuccin Latte',
    'dracula': '🧛 Dracula', 'nord': '❄️ Nord', 'tokyo-night': '🌃 Tokyo Night',
    'rose-pine': '🌹 Rosé Pine', 'everforest': '🌲 Everforest', 'kanagawa': '🌊 Kanagawa',
    'gruvbox': '🟫 Gruvbox', 'one-dark': '⚫ One Dark'
};

const THEME_SWATCHES = {
    'mocha': ['#1e1e2e', '#89b4fa', '#cba6f7'],
    'latte': ['#eff1f5', '#1e66f5', '#8839ef'],
    'dracula': ['#282a36', '#bd93f9', '#ff79c6'],
    'nord': ['#2e3440', '#88c0d0', '#b48ead'],
    'tokyo-night': ['#1a1b26', '#7aa2f7', '#bb9af7'],
    'rose-pine': ['#191724', '#c4a7e7', '#eb6f92'],
    'everforest': ['#2d353b', '#a7c080', '#d699b6'],
    'kanagawa': ['#1f1f28', '#7e9cd8', '#957fb8'],
    'gruvbox': ['#282828', '#fabd2f', '#d3869b'],
    'one-dark': ['#282c34', '#61afef', '#c678dd'],
    'auto': ['#1e1e2e', '#eff1f5', '#89b4fa']
};

function openSettings() {
    document.getElementById('settings-modal').classList.add('show');
}

function closeSettings() {
    document.getElementById('settings-modal').classList.remove('show');
}

function changeTheme(name) {
    storeSet('theme', name);
    applyTheme(name);
    syncThemeControls(name);
}

function applyTheme(name) {
    let t = name;
    if (name === 'auto') {
        t = (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches) ? 'latte' : 'mocha';
    }
    document.documentElement.setAttribute('data-theme', t);
    applyAccentHue(storeGet('accent-hue'), false);
}

// applyAccentHue переопределяет --accent пользовательским HSL.
function applyAccentHue(hue, save) {
    const root = document.documentElement;
    if (hue === null || hue === undefined || hue === '') {
        root.style.removeProperty('--accent');
        if (save) { storeDel('accent-hue'); const el = document.getElementById('accent-hue'); if (el) el.value = 210; }
        return;
    }
    root.style.setProperty('--accent', 'hsl(' + hue + ', 70%, 70%)');
    if (save) storeSet('accent-hue', hue);
    const el = document.getElementById('accent-hue');
    if (el) el.value = hue;
}

function resetAccentHue() {
    storeDel('accent-hue');
    document.documentElement.style.removeProperty('--accent');
    const el = document.getElementById('accent-hue');
    if (el) el.value = 210;
}

function syncThemeControls(name) {
    const a = document.getElementById('set-theme');
    if (a) a.value = name;
    const b = document.getElementById('theme-select');
    if (b) b.value = name;
    document.querySelectorAll('.theme-preview').forEach(el => {
        el.classList.toggle('active', el.dataset.theme === name);
    });
}

function fillThemeSelect() {
    const sel = document.getElementById('theme-select');
    if (sel) {
        sel.innerHTML = '';
        THEMES.forEach(t => {
            const opt = document.createElement('option');
            opt.value = t;
            opt.textContent = THEME_LABELS[t] || t;
            sel.appendChild(opt);
        });
        sel.value = storeGet('theme') || 'mocha';
    }
    const s2 = document.getElementById('set-theme');
    if (s2) s2.value = storeGet('theme') || 'mocha';
    renderThemePreviews();
}

function renderThemePreviews() {
    const container = document.getElementById('theme-preview-row');
    if (!container) return;
    const cur = storeGet('theme') || 'mocha';
    let html = '';
    THEMES.forEach(t => {
        const sw = THEME_SWATCHES[t] || [];
        html += '<div class="theme-preview' + (t === cur ? ' active' : '') + '" data-theme="' + t + '" title="' + (THEME_LABELS[t] || t) + '" onclick="changeTheme(\'' + t + '\')">';
        sw.forEach(c => { html += '<div style="background:' + c + '"></div>'; });
        html += '</div>';
    });
    container.innerHTML = html;
}

// ==================== УВЕДОМЛЕНИЯ ====================
let lastEventId = 0;
let unreadCount = 0;

function requestNotifPermission() {
    if ('Notification' in window && Notification.permission === 'default') {
        Notification.requestPermission();
    }
}

async function pollNotifications() {
    try {
        const r = await fetch('/api/books/history');
        const d = await r.json();
        const events = d.events || [];
        if (events.length === 0) return;
        let maxId = 0;
        for (const e of events) if (e.id > maxId) maxId = e.id;
        if (lastEventId === 0) { lastEventId = maxId; return; }
        const newEvents = events.filter(e => e.id > lastEventId);
        if (newEvents.length === 0) return;
        lastEventId = maxId;
        for (const e of newEvents) showEventToast(e);
    } catch (e) {}
}

function showEventToast(e) {
    const text = (e.type || 'событие') + ': ' + prettyPayload(e.type, e.payload);
    notify(text);
    unreadCount++;
    const c = document.getElementById('notif-count');
    c.textContent = unreadCount;
    c.style.display = 'flex';
    if ('Notification' in window && Notification.permission === 'granted') {
        try { new Notification('NAS: ' + e.type, { body: prettyPayload(e.type, e.payload) }); } catch (err) {}
    }
    addNotifItem(e);
}

function addNotifItem(e) {
    const panel = document.getElementById('notif-panel');
    const el = document.createElement('div');
    el.style.cssText = 'padding:8px 10px; border-bottom:1px solid var(--bg-button); font-size:12px;';
    el.innerHTML = '<div style="color:var(--accent);font-weight:600;">' + escapeHtml(e.type || '') + '</div><div style="color:var(--fg-muted);">' + escapeHtml(prettyPayload(e.type, e.payload)) + '</div>';
    panel.prepend(el);
    while (panel.children.length > 50) panel.removeChild(panel.lastChild);
}

function toggleNotifPanel() {
    const panel = document.getElementById('notif-panel');
    const isHidden = panel.style.display === 'none';
    panel.style.display = isHidden ? 'block' : 'none';
    if (isHidden) {
        unreadCount = 0;
        const c = document.getElementById('notif-count');
        c.textContent = '';
        c.style.display = 'none';
    }
}

// ==================== ПЛЕЙЛИСТЫ ФИЛЬМОВ ====================
function showMediaPlaylists(event) {
    document.querySelectorAll('.media-tab').forEach(t => t.classList.remove('active'));
    if (event && event.target) event.target.classList.add('active');
    document.getElementById('media-grid').style.display = 'none';
    document.getElementById('media-playlists').style.display = '';
    loadMoviePlaylists();
}

async function loadMoviePlaylists() {
    const el = document.getElementById('media-playlists');
    el.innerHTML = '<div class="loading" style="grid-column:1/-1;">Загрузка...</div>';
    try {
        const r = await apiFetch('/api/media/movie-playlists');
        const d = await r.json();
        const list = d.playlists || [];
        let html = '<div style="grid-column:1/-1;margin-bottom:8px;"><button onclick="createMoviePlaylist()">➕ Создать плейлист</button></div>';
        if (!list.length) { el.innerHTML = html + '<div class="loading" style="grid-column:1/-1;">Нет плейлистов</div>'; return; }
        list.forEach(p => {
            html += '<div class="media-card">';
            html += '<div class="media-poster-placeholder" style="cursor:pointer;" onclick="openMoviePlaylist(' + p.id + ')">📋</div>';
            html += '<div style="padding:8px 10px;"><div class="media-title">' + escapeHtml(p.name) + '</div>';
            html += '<div class="media-meta">' + (p.count || 0) + ' фильмов</div>';
            html += '<div style="display:flex;gap:4px;margin-top:6px;">';
            html += '<button onclick="openMoviePlaylist(' + p.id + ')" style="flex:1;font-size:11px;">▶️ Открыть</button>';
            html += '<button onclick="deleteMoviePlaylist(' + p.id + ')" style="font-size:11px;background:var(--error);color:var(--bg);">🗑️</button>';
            html += '</div></div></div>';
        });
        el.innerHTML = html;
    } catch (e) { el.innerHTML = '<div class="error" style="grid-column:1/-1;">Ошибка: ' + e.message + '</div>'; }
}

async function createMoviePlaylist() {
    const name = prompt('Название плейлиста:');
    if (!name) return;
    try {
        await apiFetch('/api/media/movie-playlists', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({name})});
        loadMoviePlaylists();
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function deleteMoviePlaylist(id) {
    if (!confirm('Удалить плейлист?')) return;
    await apiFetch('/api/media/movie-playlists/' + id, {method:'DELETE'});
    loadMoviePlaylists();
}

async function openMoviePlaylist(id) {
    const el = document.getElementById('media-playlists');
    el.innerHTML = '<div class="loading" style="grid-column:1/-1;">Загрузка...</div>';
    try {
        const r = await apiFetch('/api/media/movie-playlists/' + id);
        const d = await r.json();
        const movies = d.movies || [];
        let html = '<div style="grid-column:1/-1;display:flex;align-items:center;gap:10px;margin-bottom:8px;flex-wrap:wrap;">';
        html += '<button onclick="loadMoviePlaylists()">← Назад</button>';
        html += '<b style="color:var(--accent);">' + escapeHtml(d.name || '') + '</b>';
        html += '<span style="color:var(--fg-dim);font-size:12px;">' + movies.length + '</span>';
        if (movies.length) html += '<button onclick="playMoviePlaylist(' + id + ')" style="margin-left:auto;">▶️ Играть всё</button>';
        html += '</div>';
        if (!movies.length) { el.innerHTML = html + '<div class="loading" style="grid-column:1/-1;">Пусто</div>'; return; }
        movies.forEach(m => {
            const poster = m.poster_url ? '<img class="media-poster" src="/api/media/poster?url=' + encodeURIComponent(m.poster_url) + '" loading="lazy">' : '<div class="media-poster-placeholder">🎬</div>';
            html += '<div class="media-card">' + poster + '<div style="padding:8px 10px;"><div class="media-title">' + escapeHtml(m.title||'') + '</div>';
            html += '<div class="media-meta">' + (m.year||'—') + '</div>';
            html += '<button onclick="removeFromPlaylist(' + id + ',' + m.id + ')" style="margin-top:6px;width:100%;font-size:11px;background:var(--error);color:var(--bg);">🗑️ Убрать</button></div></div>';
        });
        el.innerHTML = html;
    } catch (e) { el.innerHTML = '<div class="error" style="grid-column:1/-1;">Ошибка: ' + e.message + '</div>'; }
}

async function removeFromPlaylist(pid, mid) {
    await apiFetch('/api/media/movie-playlists/' + pid + '/items/' + mid, {method:'DELETE'});
    openMoviePlaylist(pid);
}

async function playMoviePlaylist(id) {
    try {
        const r = await apiFetch('/api/media/movie-playlists/' + id);
        const d = await r.json();
        const movies = d.movies || [];
        const first = movies.find(m => m.media_type === 'movie') || movies[0];
        if (first) { closeMediaModal(); mediaType = 'movie'; openMediaModal(first.id); }
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function promptAddToPlaylist() {
    if (!currentMediaId) return;
    try {
        const r = await apiFetch('/api/media/movie-playlists');
        const d = await r.json();
        const list = d.playlists || [];
        let msg = 'Выберите номер плейлиста:\n';
        list.forEach((p,i) => { msg += (i+1) + '. ' + p.name + ' (' + p.count + ')\n'; });
        msg += '0 — создать новый';
        const ans = prompt(msg);
        if (ans === null) return;
        const n = parseInt(ans, 10);
        if (n === 0) { await createMoviePlaylistFor(currentMediaId); return; }
        const p = list[n-1];
        if (!p) return;
        const rr = await apiFetch('/api/media/movie-playlists/' + p.id + '/add', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({media_id: currentMediaId})});
        if (rr.ok) notify('➕ Добавлено в ' + p.name); else notify('❌ Ошибка', true);
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function createMoviePlaylistFor(mediaId) {
    const name = prompt('Название нового плейлиста:');
    if (!name) return;
    const r = await apiFetch('/api/media/movie-playlists', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({name})});
    const d = await r.json();
    if (d.id) {
        await apiFetch('/api/media/movie-playlists/' + d.id + '/add', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({media_id: mediaId})});
        notify('➕ Плейлист создан');
    }
}

// ==================== ПОДПИСКИ НА СЕРИАЛЫ ====================
async function subscribeSeries() {
    if (!currentSeriesId) return;
    try {
        const r = await apiFetch('/api/media/subscriptions', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({series_id: currentSeriesId})});
        const d = await r.json();
        if (r.ok) notify('🔔 Подписка оформлена: ' + (d.title || '')); else notify('❌ ' + (d.error||''), true);
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function loadSubscriptions() {
    const el = document.getElementById('subscriptions-list');
    if (!el) return;
    try {
        const r = await apiFetch('/api/media/subscriptions');
        const d = await r.json();
        const items = d.items || [];
        if (!items.length) { el.innerHTML = '<div style="color:var(--fg-dim);font-size:13px;">Нет подписок</div>'; return; }
        let html = '';
        items.forEach(s => {
            html += '<div class="playlist-row"><span class="pl-name">📺 ' + escapeHtml(s.title) + '</span>';
            html += '<button onclick="checkSubscription(' + s.id + ')">🔄</button>';
            html += '<button class="danger" onclick="unsubscribe(' + s.id + ')">🗑️</button></div>';
        });
        el.innerHTML = html;
    } catch (e) { el.innerHTML = '<div style="color:var(--error);font-size:12px;">Ошибка: ' + e.message + '</div>'; }
}

async function unsubscribe(id) {
    await apiFetch('/api/media/subscriptions/' + id, {method:'DELETE'});
    loadSubscriptions();
}

async function checkSubscription(id) {
    notify('🔄 Проверяю новые серии...');
    try {
        const r = await apiFetch('/api/media/subscriptions/' + id + '/check', {method:'POST'});
        const d = await r.json();
        notify('📺 Найдено: ' + (d.count || 0));
    } catch (e) { notify('❌ ' + e.message, true); }
}

// ==================== СТАТИСТИКА СКАЧИВАНИЙ ====================
async function loadDownloadStats() {
    const el = document.getElementById('download-stats');
    if (!el) return;
    try {
        const r = await apiFetch('/api/history/stats/downloads?days=30');
        const d = await r.json();
        const daily = d.daily || [];
        const catsR = await apiFetch('/api/history/stats/categories');
        const cats = (await catsR.json()).categories || [];
        let max = 1; daily.forEach(x => { if (x.count > max) max = x.count; });
        let html = '<h3 style="margin-top:0;color:var(--accent);">📊 Статистика скачиваний</h3>';
        html += '<div style="color:var(--fg-muted);font-size:13px;margin-bottom:10px;">Всего: <b>' + (d.all_time||0) + '</b> · за ' + (d.days||30) + ' дн.: <b>' + (d.total||0) + '</b></div>';
        if (daily.length) {
            html += '<div style="display:flex;align-items:flex-end;gap:4px;height:80px;">';
            daily.forEach(x => {
                const h = Math.max(4, Math.round((x.count/max)*70));
                html += '<div title="' + x.date + ': ' + x.count + '" style="flex:1;min-width:6px;height:' + h + 'px;background:var(--accent);border-radius:3px;"></div>';
            });
            html += '</div>';
        }
        if (cats.length) {
            html += '<div style="margin-top:12px;display:flex;flex-wrap:wrap;gap:10px;font-size:13px;">';
            cats.forEach(c => { html += '<span style="color:var(--fg-muted);">' + escapeHtml(c.category) + ': <b>' + c.count + '</b></span>'; });
            html += '</div>';
        }
        el.innerHTML = html;
    } catch (e) { el.innerHTML = '<div style="color:var(--fg-dim);font-size:12px;">Статистика недоступна</div>'; }
}

// ==================== БОНУСЫ ====================
async function rollRandomMovie() {
    try {
        const r = await apiFetch('/api/media/smart/unwatched');
        const d = await r.json();
        const items = (d.items || []).filter(x => x.media_type === 'movie');
        if (!items.length) { notify('Все фильмы просмотрены!'); return; }
        const m = items[Math.floor(Math.random() * items.length)];
        mediaType = 'movie';
        openMediaModal(m.id);
    } catch (e) { notify('❌ ' + e.message, true); }
}

// ==================== АВТО-ПЕРЕИМЕНОВАНИЕ (v8-Block8) ====================
function openRenameModal() {
    document.getElementById('rename-modal').classList.add('show');
    document.getElementById('rename-results').innerHTML = '';
    document.getElementById('rename-preview').innerHTML = '';
    const inp = document.getElementById('rename-search');
    if (inp) { inp.value = ''; setTimeout(() => inp.focus(), 50); }
}
function closeRenameModal() { document.getElementById('rename-modal').classList.remove('show'); }

async function renameSearch() {
    const q = document.getElementById('rename-search').value.trim();
    if (!q) return;
    const el = document.getElementById('rename-results');
    el.innerHTML = '<div class="loading">Поиск...</div>';
    try {
        const r = await apiFetch('/api/media/movies?q=' + encodeURIComponent(q) + '&limit=20');
        const d = await r.json();
        const items = d.items || [];
        if (!items.length) { el.innerHTML = '<div class="loading">Ничего не найдено</div>'; return; }
        let html = '<div class="history-list">';
        items.forEach(m => {
            html += '<div class="history-item"><span style="flex:1">' + escapeHtml(m.title) + ' (' + (m.year || '—') + ')</span>' +
                '<button onclick="renamePreviewById(' + m.id + ')">👁 Preview</button></div>';
        });
        html += '</div>';
        el.innerHTML = html;
    } catch (e) { el.innerHTML = '<div class="error">Ошибка: ' + escapeHtml(e.message) + '</div>'; }
}

async function renamePreviewById(id) {
    currentMediaId = id;
    try {
        const r = await apiFetch('/api/media/rename/preview', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({id})});
        const p = await r.json();
        if (p.error) { notify('❌ ' + p.error, true); return; }
        const E = escapeHtml;
        let html = '<div class="history-list">';
        html += '<div class="history-item"><span style="flex:1">' + E(p.current_path) + '</span></div>';
        html += '<div class="history-item"><span style="flex:1;color:var(--success)">→ ' + E(p.suggested_path) + '</span></div>';
        if (p.exists) html += '<div class="history-item" style="color:var(--warning)">⚠️ Целевой путь уже существует</div>';
        if (p.episodes && p.episodes.length) html += '<div class="history-item">Эпизодов: ' + p.episodes.length + '</div>';
        html += '</div>';
        html += '<div style="display:flex;gap:8px;margin-top:10px;"><button onclick="renameApply(' + id + ')">✅ Применить</button><button onclick="closeRenameModal()">❌ Отмена</button></div>';
        document.getElementById('rename-preview').innerHTML = html;
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function renameApply(id) {
    if (!confirm('Применить переименование?')) return;
    try {
        const r = await adminFetch('/api/media/rename/apply', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({id})});
        const d = await r.json();
        if (d.error) { notify('❌ ' + d.error, true); return; }
        notify('✏️ Переименовано');
        closeRenameModal();
        loadMedia();
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function loadBackgroundPhoto() {
    try {
        const r = await apiFetch('/api/media/photos/random');
        const d = await r.json();
        if (d.url) {
            document.body.style.setProperty('--bg-photo', "url('" + d.url + "')");
            document.body.classList.add('photo-background');
        }
    } catch (e) {}
}

async function musicRadioNext() {
    try {
        const r = await apiFetch('/api/music/radio/next');
        const d = await r.json();
        if (d.status === 'ok') notify('📻 Моя волна: ' + (d.title || d.artist || ''));
        else if (d.status === 'no_more') notify('📻 Больше треков этого артиста нет');
        else notify('❌ ' + (d.error || 'нет трека'), true);
        musicLoadStatus();
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function loadMusicStats() {
    const el = document.getElementById('music-stats');
    if (!el) return;
    el.innerHTML = '<div class="loading">Загрузка...</div>';
    try {
        const r = await apiFetch('/api/music/stats');
        const d = await r.json();
        const artists = d.artists || [];
        const tracks = d.tracks || [];
        let html = '<div style="display:flex;flex-wrap:wrap;gap:24px;">';
        html += '<div style="flex:1;min-width:240px;"><h4 style="color:var(--accent);">🎤 Топ артистов</h4>';
        if (!artists.length) html += '<div class="loading">Пока нет данных</div>';
        artists.forEach((a,i) => { html += '<div style="font-size:13px;padding:3px 0;">' + (i+1) + '. ' + escapeHtml(a.name) + ' <b style="float:right;">' + a.count + '</b></div>'; });
        html += '</div>';
        html += '<div style="flex:1;min-width:240px;"><h4 style="color:var(--accent);">🎵 Топ треков</h4>';
        if (!tracks.length) html += '<div class="loading">Пока нет данных</div>';
        tracks.forEach((t,i) => { html += '<div style="font-size:13px;padding:3px 0;">' + (i+1) + '. ' + escapeHtml(t.name) + ' <b style="float:right;">' + t.count + '</b></div>'; });
        html += '</div></div>';
        el.innerHTML = html;
        loadMusicStatsPg();
    } catch (e) { el.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}

// Статистика из PostgreSQL (heatmap, топы, по дням).
async function loadMusicStatsPg() {
    const el = document.getElementById('music-stats-pg');
    if (!el) return;
    el.innerHTML = '<div class="loading">Загрузка статистики...</div>';
    try {
        const [daily, hourly, artists, albums, tracks] = await Promise.all([
            apiFetch('/api/music/listens/stats/daily?days=30').then(r => r.json()).catch(() => ({})),
            apiFetch('/api/music/listens/stats/hourly').then(r => r.json()).catch(() => ({})),
            apiFetch('/api/music/listens/stats/artists?period=month').then(r => r.json()).catch(() => ({})),
            apiFetch('/api/music/listens/stats/albums?period=month').then(r => r.json()).catch(() => ({})),
            apiFetch('/api/music/listens/stats/tracks?period=month').then(r => r.json()).catch(() => ({})),
        ]);
        let html = '';
        const grid = (hourly && hourly.data) || [];
        const dayNames = ['Пн','Вт','Ср','Чт','Пт','Сб','Вс'];
        let max = 1;
        grid.forEach(row => (row || []).forEach(v => { if (v > max) max = v; }));
        html += '<h4 style="color:var(--accent);margin-top:18px;">🔥 Heatmap (день × час)</h4>';
        html += '<div style="font-family:monospace;font-size:11px;overflow-x:auto;">';
        for (let dd = 0; dd < 7; dd++) {
            html += '<div style="white-space:nowrap;">' + dayNames[dd] + ' ';
            for (let h = 0; h < 24; h++) {
                const v = (grid[dd] && grid[dd][h]) || 0;
                const op = v === 0 ? 0.08 : Math.min(1, 0.2 + v / max);
                html += '<span title="' + dayNames[dd] + ' ' + h + ':00 — ' + v + '" style="display:inline-block;width:10px;height:14px;margin:1px;border-radius:2px;background:var(--accent);opacity:' + op.toFixed(2) + ';"></span>';
            }
            html += '</div>';
        }
        html += '</div>';
        const renderTop = (title, items) => {
            let h = '<div style="flex:1;min-width:220px;"><h4 style="color:var(--accent);">' + title + '</h4>';
            if (!items || !items.length) h += '<div class="loading">Нет данных</div>';
            (items || []).forEach((x, i) => {
                h += '<div style="font-size:13px;padding:3px 0;border-bottom:1px solid var(--glass-border);">' + (i+1) + '. ' + escapeHtml(x.name) + ' <b style="float:right;">' + x.count + '</b></div>';
            });
            return h + '</div>';
        };
        html += '<div style="display:flex;flex-wrap:wrap;gap:24px;margin-top:18px;">';
        html += renderTop('🎤 Топ артистов (мес)', artists.items);
        html += renderTop('💿 Топ альбомов (мес)', albums.items);
        html += renderTop('🎵 Топ треков (мес)', tracks.items);
        html += '</div>';
        const dd2 = (daily && daily.data) || [];
        if (dd2.length) {
            let dmax = 1;
            dd2.forEach(x => { if (x.count > dmax) dmax = x.count; });
            html += '<h4 style="color:var(--accent);margin-top:18px;">📅 По дням (30)</h4><div style="display:flex;align-items:flex-end;gap:2px;height:80px;">';
            dd2.forEach(x => {
                const bh = Math.max(2, Math.round(x.count / dmax * 76));
                html += '<div title="' + x.date + ': ' + x.count + '" style="flex:1;min-width:4px;height:' + bh + 'px;background:var(--accent);border-radius:2px;opacity:0.7;"></div>';
            });
            html += '</div>';
        }
        el.innerHTML = html;
    } catch (e) { el.innerHTML = '<div class="error">Ошибка статистики: ' + e.message + '</div>'; }
}

// Умные плейлисты
let smartPlaylistType = 'recent';
async function loadSmartPlaylist(type) {
    smartPlaylistType = type || 'recent';
    const el = document.getElementById('smart-playlist');
    if (!el) return;
    el.innerHTML = '<div class="loading">Загрузка...</div>';
    try {
        const r = await apiFetch('/api/music/playlists/smart?type=' + encodeURIComponent(smartPlaylistType) + '&limit=50');
        const d = await r.json();
        const tracks = d.tracks || [];
        if (!tracks.length) { el.innerHTML = '<div class="loading">Пусто (послушайте музыку, чтобы набрать статистику)</div>'; return; }
        let html = '<div class="album-tracks">';
        tracks.forEach(t => {
            const f = String(t.track_path || '');
            html += '<div class="album-track" data-music-action="play-file" data-file="' + escAttr(f) + '">';
            html += '<span class="track-title">' + escapeHtml(t.title || f.split('/').pop()) + '</span>';
            html += '<span class="track-duration">' + escapeHtml(t.artist || '') + (t.count ? ' · ' + escapeHtml(t.count) : '') + '</span>';
            html += '<span class="track-actions">' + addPlBtn(f) + '</span>';
            html += '</div>';
        });
        html += '</div>';
        el.innerHTML = html;
    } catch (e) { el.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}


// ==================== ЗДОРОВЬЕ СЕРВИСОВ (v8-Block1) ====================
async function loadHealth() {
    try {
        const r = await apiFetch('/api/scheduler/health/services');
        const d = await r.json();
        const sum = d.summary || {up: 0, down: 0, total: 0};
        document.getElementById('health-summary').innerHTML =
            '<div class="row"><span class="label">Всего</span><span class="value">' + sum.total + '</span></div>' +
            '<div class="row"><span class="label">✅ Up</span><span style="color:var(--success)">' + sum.up + '</span></div>' +
            '<div class="row"><span class="label">❌ Down</span><span style="color:var(--error)">' + sum.down + '</span></div>';
        let html = '<div class="history-list">';
        (d.services || []).forEach(s => {
            const emoji = s.status === 'up' ? '✅' : '❌';
            const color = s.status === 'up' ? 'var(--success)' : 'var(--error)';
            html += '<div class="history-item">' +
                '<span style="flex:1">' + emoji + ' ' + escapeHtml(s.name) + '</span>' +
                '<span style="color:' + color + '">' + s.response_ms + 'ms</span>' +
                '</div>';
        });
        html += '</div>';
        document.getElementById('health-services').innerHTML = html;
    } catch (e) {
        console.error('loadHealth:', e);
    }
}

// ==================== ГЛОБАЛЬНЫЙ ПОИСК ====================
function openGlobalSearch() {
    document.getElementById('search-modal').classList.add('show');
    setTimeout(() => document.getElementById('global-search-input').focus(), 100);
}

function closeGlobalSearch() {
    document.getElementById('search-modal').classList.remove('show');
}

async function runGlobalSearch() {
    const q = document.getElementById('global-search-input').value.trim();
    if (!q) return;
    const res = document.getElementById('global-search-results');
    res.innerHTML = '<div class="loading">Поиск...</div>';
    try {
        const r = await fetch('/api/search-all?q=' + encodeURIComponent(q));
        const d = await r.json();
        renderGlobalResults(d);
    } catch (e) {
        res.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

function goToTab(name) {
    showTab(name);
}

function renderGlobalResults(d) {
    const res = document.getElementById('global-search-results');
    const sections = [
        { key: 'movies', label: '🎬 Фильмы', type: 'movie' },
        { key: 'tv', label: '📺 Сериалы', type: 'tv' },
        { key: 'torrents', label: '📋 Торренты', type: 'torrent' },
        { key: 'books', label: '📚 Книги', type: 'book' }
    ];
    let html = '';
    for (const s of sections) {
        const items = d[s.key] || [];
        if (!items.length) continue;
        html += '<div style="margin-bottom:12px;"><div style="color:var(--accent);font-weight:600;margin-bottom:6px;">' + s.label + ' (' + items.length + ')</div>';
        items.slice(0, 8).forEach(it => {
            if (s.key === 'movies' || s.key === 'tv') {
                const poster = it.poster_url ? '<img src="/api/media/poster?url=' + encodeURIComponent(it.poster_url) + '" class="gs-poster" loading="lazy">' : '';
                html += '<div class="gs-item" onclick="openSearchResult(\'' + s.type + '\', ' + (it.id || 0) + ')">' + poster +
                    '<div class="gs-info"><div class="gs-title">' + escapeHtml(it.title || '') + (it.year ? ' (' + it.year + ')' : '') + '</div>' +
                    (it.rating ? '<div class="gs-meta">⭐ ' + it.rating + '</div>' : '') + '</div></div>';
            } else if (s.key === 'torrents') {
                const mag = escapeHtml(it.magnet || it.downloadUrl || '');
                const ttitle = escapeHtml(it.title || '');
                html += '<div class="gs-item"><div class="gs-info"><div class="gs-title">' + escapeHtml(it.title || '') + '</div>' +
                    '<div class="gs-meta">' + (it.indexer || '') + ' · ' + formatSize(it.size) + ' · ' + (it.seeders || 0) + ' 🌱</div></div>' +
                    '<div class="gs-actions"><button data-mag="' + mag + '" data-ttl="' + ttitle + '" onclick="event.stopPropagation(); quickDownloadTorrent(this.dataset.mag,this.dataset.ttl)" title="Скачать">📥</button><button data-mag="' + mag + '" onclick="event.stopPropagation(); copyMagnet(this.dataset.mag)" title="Копировать">📋</button></div></div>';
            } else if (s.key === 'books') {
                const fmts = it.formats || {};
                let btns = '';
                const bkTitle = escapeHtml(it.title || '');
                if (fmts.epub) btns += '<button data-bk-title="' + bkTitle + '" data-bk-url="' + escapeHtml(fmts.epub) + '" onclick="event.stopPropagation(); downloadBook(this.dataset.bkTitle,this.dataset.bkUrl,\'epub\')">EPUB</button>';
                if (fmts.fb2) btns += '<button data-bk-title="' + bkTitle + '" data-bk-url="' + escapeHtml(fmts.fb2) + '" onclick="event.stopPropagation(); downloadBook(this.dataset.bkTitle,this.dataset.bkUrl,\'fb2\')">FB2</button>';
                html += '<div class="gs-item"><div class="gs-info"><div class="gs-title">' + escapeHtml(it.title || '') + '</div><div class="gs-meta">' + escapeHtml(it.author || '') + '</div></div><div class="gs-actions">' + btns + '</div></div>';
            }
        });
        html += '</div>';
    }
    if (!html) html = '<div class="loading">Ничего не найдено</div>';
    res.innerHTML = html;
}

function openSearchResult(type, id) {
    closeGlobalSearch();
    if (type === 'movie' || type === 'tv') {
        mediaType = type;
        goToTab('media');
        setTimeout(() => showMediaDetails(id), 200);
    }
}

async function quickDownloadTorrent(magnet, title) {
    const sel = document.getElementById('dest-select');
    const dest = sel ? sel.value : '';
    const payload = {destination: dest};
    if (String(magnet).startsWith('magnet:')) payload.magnet = magnet;
    else payload.torrentUrl = magnet;
    try {
        const r = await fetch('/api/torrents', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)});
        const d = await r.json();
        if (d.error) { notify('❌ ' + d.error, true); return; }
        let msg = '📥 Торрент добавлен';
        if (title) msg += ': ' + String(title).substring(0, 40);
        if (d.auto && d.category) msg += ' → ' + d.category;
        notify(msg);
    } catch (e) { notify('❌ ' + e.message, true); }
}

function copyMagnet(magnet) {
    navigator.clipboard.writeText(magnet).then(() => notify('📋 Magnet скопирован')).catch(() => {});
}

document.addEventListener('keydown', (e) => {
    const tgt = (e.target && e.target.tagName ? e.target.tagName : '').toLowerCase();
    const typing = tgt === 'input' || tgt === 'textarea' || tgt === 'select';

    // Громкость (Ctrl+Up/Down) — глобально
    if ((e.ctrlKey || e.metaKey) && e.key === 'ArrowUp') { e.preventDefault(); musicVolumeUp(); return; }
    if ((e.ctrlKey || e.metaKey) && e.key === 'ArrowDown') { e.preventDefault(); musicVolumeDown(); return; }

    // Хоткеи музыкального плеера (вкладка Музыка, вне полей ввода)
    if (currentTab === 'music' && !typing && !e.ctrlKey && !e.metaKey && !e.altKey) {
        switch (e.key) {
            case ' ': e.preventDefault(); musicPlayPause(); return;
            case 'ArrowRight': e.preventDefault(); if (typeof musicSeek === 'function') musicSeek(+10); return;
            case 'ArrowLeft': e.preventDefault(); if (typeof musicSeek === 'function') musicSeek(-10); return;
            case 'n': case 'N': musicNext(); return;
            case 'p': case 'P': musicPrev(); return;
            case 'm': case 'M': musicMute(); return;
            case 'l': case 'L': musicShowLyrics(); return;
            case 'f': case 'F': toggleFavorite(); return;
        }
    }

    // Командная палитра (Ctrl/Cmd+Shift+P)
    if ((e.ctrlKey || e.metaKey) && e.shiftKey && (e.key === 'P' || e.key === 'p')) {
        e.preventDefault();
        openCommandPalette();
        return;
    }
    // Настройки (Ctrl+,)
    if ((e.ctrlKey || e.metaKey) && e.key === ',') {
        e.preventDefault();
        openSettings();
        return;
    }
    // Глобальный поиск (Ctrl+K)
    if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
        e.preventDefault();
        openGlobalSearch();
        return;
    }
    // История (Ctrl+H)
    if ((e.ctrlKey || e.metaKey) && (e.key === 'h' || e.key === 'H')) {
        e.preventDefault();
        showTab('history');
        return;
    }
    // Музыка (Ctrl+M)
    if ((e.ctrlKey || e.metaKey) && (e.key === 'm' || e.key === 'M')) {
        e.preventDefault();
        showTab('music');
        return;
    }
    if (e.key === 'Escape') {
        if (document.getElementById('command-palette').classList.contains('show')) {
            closeCommandPalette();
            return;
        }
        closeAllModals();
        return;
    }
    // Переход на вкладку 1-9
    if (!typing && !e.ctrlKey && !e.metaKey && !e.altKey && e.key >= '1' && e.key <= '9') {
        const btns = Array.from(document.querySelectorAll('#tabs-nav .tab'));
        const idx = parseInt(e.key, 10) - 1;
        if (btns[idx]) { e.preventDefault(); btns[idx].click(); return; }
    }
    const playerOpen = document.getElementById('player-modal').classList.contains('show');
    if (!playerOpen) return;
    const tag = tgt;
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return;
    const v = document.getElementById('player-video');
    switch (e.key) {
        case ' ':
            e.preventDefault();
            if (v.paused) v.play(); else v.pause();
            break;
        case 'f':
        case 'F':
            e.preventDefault();
            if (document.fullscreenElement) document.exitFullscreen();
            else document.getElementById('player-modal').requestFullscreen();
            break;
        case 'ArrowRight':
            e.preventDefault();
            v.currentTime = Math.min(v.duration || 0, v.currentTime + 10);
            break;
        case 'ArrowLeft':
            e.preventDefault();
            v.currentTime = Math.max(0, v.currentTime - 10);
            break;
    }
});

function closeAllModals() {
    document.querySelectorAll('.modal.show').forEach(m => m.classList.remove('show'));
    const v = document.getElementById('player-video');
    if (v) v.pause();
    const np = document.getElementById('notif-panel');
    if (np) np.style.display = 'none';
}


// ==================== НАСТРОЙКИ ====================
function renderRoadmap() {
    const roadmap = {
        done: ['Медиа-центр с постерами TMDb', 'HTML5-плеер + транскод', 'YouTube 4K', 'Музыка MPD (963 альбома)', 'Торренты (Prowlarr)', 'Книги (Flibusta, 874k) + читалка FB2/EPUB', 'Обложки альбомов (локальные + iTunes)', 'Кэш дисков и фото (SSD)', 'MPD auto-install + browser fallback', '1-Click установка и Wizard', 'Мультипрофили + PIN', 'DLNA', 'ntfy-уведомления', 'Бэкапы и self-healing', 'Темы (5 + auto)', 'PWA'],
        in_progress: ['Синхронизация «Продолжить просмотр»', 'Умный поиск (жанры/актёры/год)'],
        planned: ['Дашборд «Сегодня»: новые виджеты', 'Telegram-уведомления', 'Интеграция с играми (Steam/Lutris)', 'Авто-теги музыки (MusicBrainz Picard)', 'Трейлеры (YouTube inline)', 'Мобильный пульт (PWA)', 'Умные альбомы фото (YOLO/CLIP)', 'Кинокарта (D3.js)', 'Bit-perfect MPD (USB-ЦАП)', 'Snapcast мультирум', 'reMarkable интеграция']
    };
    let html = '<h4 style="color:var(--success)">✅ Сделано</h4><ul>';
    roadmap.done.forEach(i => html += '<li>' + i + '</li>');
    html += '</ul><h4 style="color:var(--warning)">🚧 В работе</h4><ul>';
    roadmap.in_progress.forEach(i => html += '<li>' + i + '</li>');
    html += '</ul><h4 style="color:var(--fg-muted)">📋 Планируется</h4><ul>';
    roadmap.planned.forEach(i => html += '<li>' + i + '</li>');
    html += '</ul>';
    document.getElementById('roadmap').innerHTML = html;
}

// ==================== БЭКАПЫ (v8-Block5) ====================
async function loadBackups() {
    const el = document.getElementById('backup-list');
    if (!el) return;
    el.innerHTML = '<div class="loading">Загрузка...</div>';
    try {
        const r = await apiFetch('/api/scheduler/backup/list');
        const d = await r.json();
        let html = '';
        ['daily', 'weekly', 'monthly'].forEach(t => {
            const arr = d[t] || [];
            if (!arr.length) return;
            html += '<h4 style="color:var(--accent);margin:8px 0 4px;">' + t + ' (' + arr.length + ')</h4><div class="history-list">';
            arr.forEach(b => {
                const displayName = b.name || b.file;
                html += '<div class="history-item">' +
                    '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">' + escapeHtml(displayName) + '</span>' +
                    '<span>' + formatSize(b.size_bytes) + '</span>' +
                    '<button class="restore-btn" data-file="' + escapeHtml(b.file) + '" style="margin-left:8px;">↩️</button>' +
                    '</div>';
            });
            html += '</div>';
        });
        el.innerHTML = html || '<div style="color:var(--fg-dim);">Бэкапов нет</div>';
        el.querySelectorAll('.restore-btn').forEach(btn => {
            btn.addEventListener('click', () => restoreBackup(btn.dataset.file));
        });
    } catch (e) { el.innerHTML = '<div class="error">Ошибка: ' + escapeHtml(e.message) + '</div>'; }
}

async function runBackup() {
    try {
        await adminFetch('/api/scheduler/backup/run', {method: 'POST'});
        notify('💾 Бэкап запущен');
        setTimeout(loadBackups, 5000);
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function restoreBackup(file) {
    if (!confirm('Восстановить ' + file + '?\n\n⚠️ Это перезапишет текущие данные БД!')) return;
    try {
        const r = await adminFetch('/api/scheduler/backup/restore', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({file: file, confirm: true})
        });
        const d = await r.json();
        notify(d.error ? ('❌ ' + d.error) : ('♻️ Восстановлено: ' + (d.status || 'ok')), !!d.error);
    } catch (e) { notify('❌ ' + e.message, true); }
}

// ==================== ОЧИСТКА ДИСКА (v8-Block4) ====================
async function previewCleanup() {
    const el = document.getElementById('cleanup-preview');
    el.innerHTML = '<div class="loading">Считаю...</div>';
    try {
        const r = await apiFetch('/api/stats/cleanup/preview');
        const data = await r.json();
        let html = '<div class="history-list">';
        let total = 0;
        (data || []).forEach(item => {
            total += item.size_bytes || 0;
            const extra = item.count ? ' (' + item.count + ' шт.)' : '';
            html += '<div class="history-item">' +
                '<span style="flex:1">' + escapeHtml(item.description) + extra + '</span>' +
                '<span>' + (item.size_bytes ? formatSize(item.size_bytes) : '—') + '</span></div>';
        });
        html += '</div>';
        html += '<div class="row"><span class="label">Итого можно освободить</span><span style="color:var(--accent)">' + formatSize(total) + '</span></div>';
        el.innerHTML = html;
        document.getElementById('cleanup-btn').style.display = 'inline-block';
    } catch (e) { el.innerHTML = '<div class="error">Ошибка: ' + escapeHtml(e.message) + '</div>'; }
}

async function runCleanup() {
    if (!confirm('Очистить? Это удалит кэш, старые образы podman, логи, корзину.')) return;
    const btn = document.getElementById('cleanup-btn');
    btn.disabled = true; btn.textContent = '🧹 Чищу...';
    try {
        const r = await adminFetch('/api/stats/cleanup/run?dry=false', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({targets: ['podman_images', 'journald', 'cache', 'trash', 'pacman_cache']})
        });
        const data = await r.json();
        notify('🧹 Освобождено: ' + formatSize(data.freed_bytes || 0));
        previewCleanup();
    } catch (e) { notify('❌ ' + e.message, true); }
    btn.disabled = false; btn.textContent = '🧹 Очистить';
}

function populateSettings() {
    const theme = storeGet('theme') || 'mocha';
    const sel = document.getElementById('set-theme');
    if (sel) sel.value = (THEMES.includes(theme) ? theme : 'auto');
    syncThemeControls(storeGet('theme') || 'mocha');
    renderThemePreviews();
    const ant = document.getElementById('set-autonext');
    if (ant) ant.checked = storeGet('autoNext') !== 'false';
    renderRoadmap();
    renderTabsSettings();
    loadSubscriptions();
    loadBackups();
    loadEnv();
    loadUpdateStatus();
}

// ---- v9.2: редактор .env ----
async function loadEnv() {
    const el = document.getElementById('env-list');
    if (!el) return;
    el.innerHTML = '<div class="loading">Загрузка...</div>';
    try {
        const r = await adminFetch('/api/notify/env');
        const d = await r.json().catch(() => ({}));
        if (d.error) {
            el.innerHTML = '<div class="error">' + escapeHtml(d.error) +
                ' <button onclick="storeDel(\'admin_token\');loadEnv()">Повторить</button></div>';
            return;
        }
        let html = '';
        (d.vars || []).forEach(v => {
            // Маску '***' НИКОГДА не кладём в value: иначе пользователь дописывает
            // символ к маске и в .env записывается '***abc'. Секрет отдаётся
            // пустым полем + disabled, изменение — через кнопку ✏️.
            const masked = v.value === '***';
            html += '<div class="setting-row" style="gap:8px;">' +
                '<span style="min-width:180px;font-family:monospace;font-size:12px;">' + escapeHtml(v.key) + '</span>' +
                '<input type="text" data-envkey="' + escapeHtml(v.key) + '" value="' + (masked ? '' : escapeHtml(v.value)) + '"' +
                (masked ? ' placeholder="*** (скрыт)" disabled' : '') + ' style="flex:1;">' +
                (masked ? '<button type="button" data-edit-secret="1" data-env-key="' + escapeHtml(v.key) + '" title="Изменить секрет">✏️</button>' : '') +
                '<button type="button" data-env-save="' + escapeHtml(v.key) + '">💾</button></div>';
        });
        el.innerHTML = html || '<div class="loading">Нет переменных</div>';
        el.querySelectorAll('button[data-env-save]').forEach(b =>
            b.addEventListener('click', () => saveEnv(b.dataset.envSave)));
        el.querySelectorAll('button[data-edit-secret]').forEach(b =>
            b.addEventListener('click', () => editEnvSecret(b.dataset.envKey)));
    } catch (e) { el.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}
// envInput — поиск по data-атрибуту без CSS-селектора: ключ из .env не может
// инъецировать кавычку/скобку в querySelector.
function envInput(key) {
    return Array.from(document.querySelectorAll('input[data-envkey]'))
        .find(i => i.dataset.envkey === key) || null;
}
function editEnvSecret(key) {
    const input = envInput(key);
    if (!input) return;
    input.disabled = false;
    input.value = '';
    input.placeholder = 'введите новый секрет';
    input.focus();
    const row = input.closest('.setting-row');
    if (row) { const b = row.querySelector('button[data-edit-secret]'); if (b) b.remove(); }
}
async function saveEnv(key) {
    const input = envInput(key);
    if (!input) return;
    if (input.disabled) { notify('Секрет скрыт — нажмите ✏️, чтобы изменить', true); return; }
    if (input.value === '' ) { notify('Пустое значение — пропуск', true); return; }
    if (input.value === '***') { notify('Секрет скрыт — введите новое значение, чтобы изменить', true); return; }
    try {
        const r = await adminFetch('/api/notify/env', {
            method: 'POST', headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({key: key, value: input.value})
        });
        if (r.ok) notify('✅ Сохранено: ' + key + ' (сделайте backup .env.bak)');
        else notify('❌ Ошибка сохранения (нужен ADMIN_TOKEN: ' + r.status + ')', true);
    } catch (e) { notify('❌ ' + e.message, true); }
}

// ---- v9.2: обновление стека ----
async function loadUpdateStatus() {
    const el = document.getElementById('update-status');
    if (!el) return;
    try {
        const r = await apiFetch('/api/scheduler/update/status');
        const d = await r.json();
        el.textContent = 'Последнее: ' + (d.last || '—') + ' | статус: ' + (d.status || '—') + ' | commit: ' + (d.commit || '—');
    } catch (e) {}
}
async function rollbackUpdate() {
    if (!confirm('Откатить на предыдущий коммит и пересобрать?')) return;
    try {
        const r = await adminFetch('/api/notify/update/rollback', {method: 'POST'});
        notify(r.ok ? '↩️ Откат запущен' : '❌ Откат не запущен (нужен ADMIN_TOKEN: ' + r.status + ')', !r.ok);
    }
    catch (e) { notify('❌ ' + e.message, true); }
}

function exportSettings() {
    const settings = {
        theme: storeGet('theme'),
        autoNext: storeGet('autoNext'),
        selectedDestination: storeGet('selectedDestination'),
        refreshInterval: storeGet('refreshInterval'),
        tabConfig: storeGet('tabConfig')
    };
    const blob = new Blob([JSON.stringify(settings, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'nas-stack-settings.json';
    a.click();
    URL.revokeObjectURL(url);
}

function importSettings() {
    const input = document.createElement('input');
    input.type = 'file';
    input.accept = '.json';
    input.onchange = async (e) => {
        try {
            const file = e.target.files[0];
            const text = await file.text();
            const settings = JSON.parse(text);
            for (const [k, v] of Object.entries(settings)) {
                if (v !== null && v !== undefined) storeSet(k, v);
            }
            location.reload();
        } catch (err) { notify('❌ Ошибка импорта: ' + err.message, true); }
    };
    input.click();
}

function resetSettings() {
    if (!confirm('Сбросить все настройки?')) return;
    try { localStorage.clear(); } catch (e) {}
    try { sessionStorage.clear(); } catch (e) {}
    location.reload();
}

// ==================== ВКЛАДКИ (drag & drop) ====================
const DEFAULT_TABS = [
    { id: 'today', label: '🏠 Сегодня', visible: true },
    { id: 'stats', label: '📊 Метрики', visible: true },
    { id: 'torrents', label: '📋 Торренты', visible: true },
    { id: 'books', label: '📚 Книги', visible: true },
    { id: 'music', label: '🎵 Музыка', visible: true },
    { id: 'youtube', label: '📹 YouTube', visible: true },
    { id: 'media', label: '🎬 Медиа', visible: true },
    { id: 'health', label: '🩺 Здоровье', visible: true },
    { id: 'cinema', label: '🍿 Киноафиша', visible: true },
    { id: 'photos', label: '🖼️ Фото', visible: true },
    { id: 'history', label: '📜 История', visible: true },
    { id: 'settings', label: '⚙️ Настройки', visible: true, locked: true }
];

function loadTabConfig() {
    try {
        const saved = JSON.parse(localStorage.getItem('tabConfig') || 'null');
        if (saved && Array.isArray(saved) && saved.length === DEFAULT_TABS.length) return saved;
    } catch (e) {}
    return JSON.parse(JSON.stringify(DEFAULT_TABS));
}

function saveTabConfig(config) { localStorage.setItem('tabConfig', JSON.stringify(config)); }

let tabConfig = loadTabConfig();

function renderTabsNav() {
    const container = document.getElementById('tabs-nav');
    if (!container) return;
    let html = '';
    tabConfig.forEach(t => {
        if (!t.visible) return;
        const active = currentTab === t.id ? ' active' : '';
        let badge = '';
        if (t.id === 'media' && newMediaCount > 0) {
            badge = ' <span style="background:var(--error);color:var(--bg);border-radius:10px;font-size:10px;padding:1px 6px;margin-left:4px;vertical-align:middle;">' + newMediaCount + '</span>';
        }
        if (t.id === 'cinema' && cinemaReleasedCount > 0) {
            badge = ' <span style="background:var(--error);color:var(--bg);border-radius:50%;width:9px;height:9px;display:inline-block;margin-left:4px;vertical-align:middle;"></span>';
        }
        html += '<button class="tab' + active + '" data-tab="' + t.id + '" onclick="showTab(\'' + t.id + '\')">' + t.label + badge + '</button>';
    });
    container.innerHTML = html;
}

// ==================== v9: БЕЙДЖ НОВЫХ МЕДИАФАЙЛОВ ====================
let newMediaCount = 0;

async function refreshNewMediaBadge() {
    try {
        const r = await fetch('/api/media/new/count');
        const d = await r.json();
        const n = d.count || 0;
        if (n !== newMediaCount) {
            newMediaCount = n;
            renderTabsNav();
        }
    } catch (e) {}
}

function resetNewMediaBadge() {
    if (newMediaCount === 0) return;
    newMediaCount = 0;
    renderTabsNav();
    fetch('/api/media/new/reset', {method: 'POST'}).catch(() => {});
}

function renderTabsSettings() {
    const container = document.getElementById('tabs-settings');
    if (!container) return;
    let html = '';
    tabConfig.forEach((t, i) => {
        html += '<div class="tab-setting-item" draggable="true" data-tab-id="' + t.id + '" data-index="' + i + '">';
        html += '<span class="drag-handle">☰</span><span class="tab-setting-label">' + t.label + '</span>';
        if (t.locked) {
            html += '<span style="color:var(--fg-dim); font-size:11px; margin-left:auto;">(обязательна)</span>';
        } else {
            const checked = t.visible ? 'checked' : '';
            html += '<label style="margin-left:auto; display:flex; align-items:center; gap:6px; color:var(--fg-muted); font-size:12px; cursor:pointer;"><input type="checkbox" ' + checked + ' onchange="toggleTabVisibility(\'' + t.id + '\', this.checked)"> Показать</label>';
        }
        html += '</div>';
    });
    container.innerHTML = html;
    initDragAndDrop();
}

function toggleTabVisibility(id, visible) {
    const t = tabConfig.find(x => x.id === id);
    if (!t || t.locked) return;
    t.visible = visible;
    saveTabConfig(tabConfig);
    renderTabsNav();
    renderTabsSettings();
}

function resetTabOrder() {
    if (!confirm('Сбросить порядок вкладок по умолчанию?')) return;
    tabConfig = JSON.parse(JSON.stringify(DEFAULT_TABS));
    saveTabConfig(tabConfig);
    renderTabsNav();
    renderTabsSettings();
}

function initDragAndDrop() {
    const items = document.querySelectorAll('.tab-setting-item');
    let draggedItem = null;
    items.forEach(item => {
        item.addEventListener('dragstart', (e) => { draggedItem = item; item.classList.add('dragging'); e.dataTransfer.effectAllowed = 'move'; });
        item.addEventListener('dragend', () => { item.classList.remove('dragging'); draggedItem = null; updateTabConfigFromDOM(); });
        item.addEventListener('dragover', (e) => {
            e.preventDefault();
            if (!draggedItem || draggedItem === item) return;
            const items_arr = Array.from(item.parentNode.children);
            const draggedIdx = items_arr.indexOf(draggedItem);
            const targetIdx = items_arr.indexOf(item);
            if (draggedIdx < targetIdx) item.parentNode.insertBefore(draggedItem, item.nextSibling);
            else item.parentNode.insertBefore(draggedItem, item);
        });
    });
}

function updateTabConfigFromDOM() {
    const items = document.querySelectorAll('.tab-setting-item');
    const newOrder = [];
    items.forEach(item => {
        const id = item.dataset.tabId;
        const t = tabConfig.find(x => x.id === id);
        if (t) newOrder.push(t);
    });
    if (newOrder.length === tabConfig.length) {
        tabConfig = newOrder;
        saveTabConfig(tabConfig);
        renderTabsNav();
    }
}

// ==================== HEALTH DOT / RESET ====================
async function checkHealthDot() {
    const dot = document.getElementById('health-dot');
    const txt = document.getElementById('health-text');
    if (!dot || !txt) return;
    try {
        const r = await fetch('/api/scheduler/health/summary');
        const d = await r.json();
        if ((d.down || 0) > 0) { dot.style.background = 'var(--error)'; txt.textContent = 'Ошибки: ' + d.down; }
        else { dot.style.background = 'var(--success)'; txt.textContent = 'Всё OK (' + (d.up || 0) + ')'; }
    } catch (e) { dot.style.background = 'var(--warning)'; txt.textContent = 'Нет связи'; }
}
async function resetSetup() {
    if (!confirm('Удалить .env и вернуться к мастеру настройки?')) return;
    try {
        const r = await fetch('/api/setup/reset', {method: 'POST'});
        if (r.ok) { alert('Настройки сброшены. Откроется мастер.'); location.href = '/setup'; }
        else { alert('Ошибка сброса'); }
    } catch (e) { alert('Ошибка: ' + e.message); }
}

// ==================== БРАУЗЕРНЫЙ РЕЖИМ (без MPD) ====================
let musicAudioMode = 'mpd';
async function loadAudioMode() {
    try {
        const r = await fetch('/api/music/audio/mode');
        const d = await r.json();
        musicAudioMode = d.mode || 'mpd';
    } catch (e) { musicAudioMode = 'browser'; }
    const p = document.getElementById('browser-audio-panel');
    if (p) p.style.display = musicAudioMode === 'browser' ? 'block' : 'none';
    const bar = document.getElementById('music-player-bar');
    if (bar) bar.style.display = musicAudioMode === 'browser' ? 'none' : '';
}
function browserCoverURL(a, al) {
    return '/api/music/covers/' + encodeURIComponent(a) + '/' + encodeURIComponent(al) + '?v=v6';
}
async function loadBrowserAlbums() {
    const grid = document.getElementById('music-albums');
    if (!grid) return;
    grid.innerHTML = skeletonGrid(12);
    try {
        const r = await fetch('/api/music/browser/albums');
        const d = await r.json();
        const albums = d.albums || [];
        const cnt = document.getElementById('music-albums-count');
        if (cnt) cnt.textContent = '(' + albums.length + ')';
        if (!albums.length) { grid.innerHTML = '<div class="loading">Музыка не найдена в MUSIC_DIR</div>'; return; }
        grid.replaceChildren();
        albums.forEach(a => {
            const card = document.createElement('div');
            card.className = 'album-card';
            card.addEventListener('click', () => openBrowserAlbum(String(a.dir || ''), String(a.album || '')));

            const img = document.createElement('img');
            img.className = 'music-album-cover';
            img.loading = 'lazy';
            img.src = browserCoverURL(a.artist, a.album);
            img.addEventListener('error', () => {
                img.style.background = 'var(--bg-button)';
                img.removeAttribute('src');
            });

            const title = document.createElement('div');
            title.className = 'album-card-title';
            title.textContent = a.album || '';

            const meta = document.createElement('div');
            meta.style.fontSize = '11px';
            meta.style.color = 'var(--fg-dim)';
            meta.textContent = (a.artist || '') + ' · ' + (a.track_count || 0);

            card.append(img, title, meta);
            grid.appendChild(card);
        });
    } catch (e) {
        grid.replaceChildren();
        const err = document.createElement('div');
        err.className = 'error';
        err.textContent = 'Ошибка: ' + e.message;
        grid.appendChild(err);
    }
}
let browserCurrentArtist = '';
async function openBrowserAlbum(dir, album) {
    browserCurrentArtist = String(dir || '').split('/')[0] || '';
    try {
        const r = await fetch('/api/music/browser/album?dir=' + encodeURIComponent(dir));
        const d = await r.json();
        const el = document.getElementById('browser-tracks');
        if (!el) return;

        el.replaceChildren();
        const head = document.createElement('div');
        head.style.cssText = 'font-size:13px;font-weight:600;margin-bottom:6px;';
        head.textContent = d.album || album || '';
        el.appendChild(head);

        (d.tracks || []).forEach(t => {
            const row = document.createElement('div');
            row.className = 'track-row';
            row.addEventListener('click', () => playBrowserTrack(String(t.file || ''), String(t.title || '')));

            const span = document.createElement('span');
            span.className = 'track-title';
            span.textContent = t.title || t.file || '';

            row.appendChild(span);
            el.appendChild(row);
        });

        el.scrollIntoView({behavior: 'smooth', block: 'nearest'});
    } catch (e) { notify('❌ ' + e.message, true); }
}
function playBrowserTrack(file, title) {
    const a = document.getElementById('browser-audio');
    if (!a) return;
    a.src = '/api/music/browser/stream?file=' + encodeURIComponent(file);
    a.play().catch(() => {});
    const n = document.getElementById('browser-now');
    if (n) n.textContent = '▶️ ' + (title || file);
    const t = document.getElementById('browser-title');
    if (t) t.textContent = title || file;
    const ar = document.getElementById('browser-artist');
    if (ar) ar.textContent = browserCurrentArtist || '—';
    const bar = document.getElementById('music-player-bar');
    if (bar) bar.style.display = 'none';
}

// ==================== AUTO-UPDATE ====================
async function checkUpdates() {
    try {
        const r = await fetch('/api/scheduler/update/status');
        const d = await r.json();
        if ((d.updates_available || 0) > 0) {
            document.getElementById('update-count').textContent = d.updates_available;
            document.getElementById('update-banner').style.display = 'block';
            document.body.style.paddingTop = '42px';
        }
    } catch (e) {}
}
async function runUpdate() {
    if (!confirm('Обновить NAS-Stack? Это займёт 2-5 минут, стек перезапустится.')) return;
    document.getElementById('update-banner').textContent = '⏳ Обновление... Не закрывайте страницу.';
    try {
        const r = await adminFetch('/api/scheduler/update/run', {method: 'POST'});
        if (!r.ok) {
            alert('Обновление не запущено: ' + r.status + ' (нужен ADMIN_TOKEN)');
            return;
        }
        setTimeout(() => location.reload(), 60000);
    } catch (e) { alert('Ошибка обновления: ' + e.message); }
}
function dismissUpdate() {
    document.getElementById('update-banner').style.display = 'none';
    document.body.style.paddingTop = '0';
    localStorage.setItem('update-dismissed', Date.now().toString());
}

// ==================== CHROMECAST ====================
function updateCastVisibility() {
    const b = document.getElementById('cast-btn');
    if (b) b.style.display = (navigator.presentation && navigator.presentation.defaultRequest !== undefined) ? 'inline-block' : 'none';
}
function castVideo() {
    const v = document.getElementById('player-video');
    if (!v || !(v.currentSrc || v.src)) { notify('Сначала запустите видео', true); return; }
    if (!navigator.presentation) { notify('Cast не поддерживается в этом браузере', true); return; }
    const req = new PresentationRequest([v.currentSrc || v.src]);
    req.start().then(() => notify('📺 Подключено к ТВ')).catch(e => notify('❌ ' + e.message, true));
}

// ==================== ПРОФИЛИ ====================
let profilesCache = [];
async function loadProfiles() {
    try {
        const r = await fetch('/api/auth/profiles');
        const d = await r.json();
        profilesCache = d.profiles || [];
        if (!storeGet('profileId') && profilesCache.length) {
            if (profilesCache.length === 1 && !profilesCache[0].has_pin) await chooseProfile(profilesCache[0].id, true);
            else showProfileSelector();
        }
    } catch (e) {}
}
function showProfileSelector() {
    const el = document.getElementById('profile-list');
    if (!el) return;
    let html = '';
    profilesCache.forEach(p => {
        html += '<div class="profile-card" onclick="chooseProfile(' + p.id + ')" style="cursor:pointer;padding:14px 18px;background:var(--bg-card);border-radius:12px;min-width:96px;">' +
            '<div style="font-size:40px;">' + escapeHtml(p.avatar || '👤') + '</div>' +
            '<div style="margin-top:6px;">' + escapeHtml(p.name) + (p.is_kids ? ' 🧒' : '') + '</div></div>';
    });
    el.innerHTML = html || '<div class="loading">Нет профилей</div>';
    document.getElementById('profile-modal').classList.add('show');
}
async function chooseProfile(id, silent) {
    let pin = '';
    if (!silent) pin = prompt('PIN для профиля (если задан):') || '';
    try {
        const r = await fetch('/api/auth/profiles/' + id + '/select', {
            method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({pin})
        });
        if (r.ok) {
            const d = await r.json();
            // Профиль — это UI-состояние. auth-service больше не выпускает
            // токен на /select, поэтому jwt_token (admin) не подменяется.
            storeSet('profileId', String(d.profile_id));
            const p = d.profile || profilesCache.find(x => x.id === d.profile_id);
            if (p) storeSet('profileMaxRating', p.max_rating || 'R');
            const m = document.getElementById('profile-modal');
            if (m) m.classList.remove('show');
            notify('👤 Профиль выбран');
        } else if (!silent) notify('❌ Неверный PIN', true);
    } catch (e) { if (!silent) notify('❌ ' + e.message, true); }
}
async function createProfile() {
    const name = document.getElementById('new-profile-name').value.trim();
    const pin = document.getElementById('new-profile-pin').value.trim();
    if (!name) { notify('Введите имя профиля', true); return; }
    try {
        const r = await fetch('/api/auth/profiles', {
            method: 'POST', headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({name, pin})
        });
        if (r.ok) { document.getElementById('new-profile-name').value = ''; document.getElementById('new-profile-pin').value = ''; await loadProfiles(); showProfileSelector(); }
        else notify('❌ Ошибка создания профиля', true);
    } catch (e) { notify('❌ ' + e.message, true); }
}
