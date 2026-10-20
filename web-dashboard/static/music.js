// NAS Dashboard — music player, playlists, artists, MPD UI.
// Classic script: expects shared globals from app.js.

// ==================== 🎵 МУЗЫКА: САЙДБАР + АРТИСТ ====================
let musicSidebarTab = 'artists';
let musicSearchQuery = '';
let musicArtists = [];
let musicAlbumsList = [];
let musicPlaylistsList = [];
let currentArtist = null;

// escAttr — экранирование значения для data-атрибута. escapeHtml тоже
// экранирует кавычки, но падает на не-строках (числовые id/pos).
function escAttr(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, ch => (
        { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]
    ));
}

// Один делегированный обработчик вместо inline-onclick со склейкой данных:
// тег вида x" onmouseover=... больше не может выйти из атрибута.
document.addEventListener('click', (ev) => {
    const el = ev.target.closest('[data-music-action]');
    if (!el) return;
    const d = el.dataset;
    switch (d.musicAction) {
        case 'open-artist':          openArtist(d.artist); break;
        case 'open-album':           openAlbum(d.artist, d.album); break;
        case 'play-album':           playAlbumBulk(d.artist, d.album); break;
        case 'play-saved-playlist':  playSavedPlaylistSidebar(d.playlist); break;
        case 'play-artist':          playArtist(d.artist); break;
        case 'enqueue-artist':       enqueueArtist(d.artist); break;
        case 'play-file':            musicPlayFile(d.file); break;
        case 'play-playlist-file':   musicPlayPlaylistFile(d.file); break;
        case 'add-to-playlist':      openAddToPlaylist(d.file); break;
        case 'add-to-playlist-name': addToPlaylist(d.playlist); break;
        case 'show-playlist':        musicShowPlaylist(d.playlist); break;
        case 'play-playlist':        musicPlayPlaylist(d.playlist); break;
        case 'delete-playlist':      musicDeletePlaylist(d.playlist); break;
        case 'queue-play':           musicQueuePlay(d.id); break;
        case 'queue-delete':         musicQueueDelete(d.id); break;
        case 'remove-favorite':      removeFavorite(d.file); break;
    }
});

async function loadMusicSidebar() {
    const list = document.getElementById('music-sidebar-list');
    if (!list) return;
    list.innerHTML = '<div class="loading">Загрузка...</div>';
    try {
        const [ar, al, pl] = await Promise.all([
            apiFetch('/api/music/artists').then(r => r.json()).catch(() => ({})),
            apiFetch('/api/music/albums').then(r => r.json()).catch(() => ({})),
            apiFetch('/api/music/saved-playlists').then(r => r.json()).catch(() => ({})),
        ]);
        musicArtists = ar.artists || [];
        musicAlbumsList = al.albums || [];
        musicPlaylistsList = pl.playlists || [];
        renderMusicSidebar();
    } catch (e) { list.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}

function setMusicSidebarTab(tab, btn) {
    musicSidebarTab = tab;
    document.querySelectorAll('.music-filter button').forEach(b => b.classList.remove('active'));
    if (btn) btn.classList.add('active');
    renderMusicSidebar();
}

function filterMusicSidebar(q) {
    musicSearchQuery = (q || '').toLowerCase().trim();
    renderMusicSidebar();
}

function renderMusicSidebar() {
    const list = document.getElementById('music-sidebar-list');
    if (!list) return;
    const q = musicSearchQuery;
    let html = '';
    if (musicSidebarTab === 'artists') {
        const items = musicArtists.filter(a => !q || (a.name || '').toLowerCase().includes(q));
        html = items.map(a =>
            '<div class="music-list-item' + (currentArtist === a.name ? ' active' : '') + '"' +
            ' data-music-action="open-artist" data-artist="' + escAttr(a.name) + '">' +
            '<span>👤 ' + escapeHtml(a.name) + '</span><span class="badge">' + (a.track_count || 0) + '</span></div>'
        ).join('');
    } else if (musicSidebarTab === 'albums') {
        const items = musicAlbumsList.filter(a => !q ||
            (a.album || '').toLowerCase().includes(q) || (a.artist || '').toLowerCase().includes(q));
        html = items.map(a =>
            '<div class="music-list-item" data-music-action="open-album"' +
            ' data-artist="' + escAttr(a.artist) + '" data-album="' + escAttr(a.album) + '">' +
            '<span>💿 ' + escapeHtml(a.artist || '') + ' — ' + escapeHtml(a.album || '') + '</span></div>'
        ).join('');
    } else {
        const items = musicPlaylistsList.filter(p => !q || ((p.name || p) + '').toLowerCase().includes(q));
        html = items.map(p => {
            const name = p.name || p;
            return '<div class="music-list-item" data-music-action="play-saved-playlist" data-playlist="' + escAttr(name) + '">' +
                '<span>📋 ' + escapeHtml(name) + '</span></div>';
        }).join('');
    }
    list.innerHTML = html || '<div class="empty">Ничего не найдено</div>';
}

function renderTrackRow(t) {
    const file = t.file || t.path || '';
    const title = t.title || file.split('/').pop();
    const playing = currentPlayingFile && file === currentPlayingFile ? ' playing' : '';
    return '<div class="track-row' + playing + '" data-music-action="play-file"' +
        ' data-file="' + escAttr(file) + '">' +
        '<button class="track-add-pl" data-music-action="add-to-playlist"' +
        ' data-file="' + escAttr(file) + '" title="Добавить в плейлист">➕</button>' +
        '<span class="track-title">' + escapeHtml(title) + '</span>' +
        '<span class="track-duration">' + fmtMusicTime(t.duration || 0) + '</span></div>';
}

function showMusicDetail(html) {
    document.getElementById('music-main-default').style.display = 'none';
    const d = document.getElementById('music-detail');
    d.style.display = '';
    d.innerHTML = '<button class="music-detail-back" onclick="closeMusicDetail()">← Назад</button>' + html;
}
function closeMusicDetail() {
    currentArtist = null;
    document.getElementById('music-detail').style.display = 'none';
    document.getElementById('music-main-default').style.display = '';
    renderMusicSidebar();
}

async function openArtist(name) {
    currentArtist = name;
    document.getElementById('music-search').value = '';
    musicSearchQuery = '';
    renderMusicSidebar();
    let d;
    try { d = await (await apiFetch('/api/music/artist/' + encodeURIComponent(name))).json(); }
    catch (e) { notify('❌ ' + e.message, true); return; }
    const shown = d.artist || name;
    let html = '<div class="artist-header"><h2>👤 ' + escapeHtml(shown) + '</h2>' +
        '<div class="artist-stats">' + (d.track_count || 0) + ' треков · ' + (d.album_count || 0) + ' альбомов</div>' +
        '<div class="artist-actions">' +
        '<button data-music-action="play-artist" data-artist="' + escAttr(shown) + '">▶️ Играть всё</button>' +
        '<button data-music-action="enqueue-artist" data-artist="' + escAttr(shown) + '">➕ В очередь</button>' +
        '</div></div><div class="artist-albums">';
    const albums = d.albums || {};
    for (const [alb, tracks] of Object.entries(albums)) {
        html += '<div class="album-block"><h3>💿 ' + escapeHtml(cleanMetaName(alb) || alb) + '</h3><div class="track-list">' +
            tracks.map(renderTrackRow).join('') + '</div></div>';
    }
    if (!Object.keys(albums).length) html += '<div class="empty">Треков не найдено</div>';
    html += '</div>';
    showMusicDetail(html);
}

async function openAlbum(artist, album) {
    document.getElementById('music-search').value = '';
    musicSearchQuery = '';
    renderMusicSidebar();
    let d;
    try { d = await (await apiFetch('/api/music/albums/' + encodeURIComponent(artist) + '/' + encodeURIComponent(album))).json(); }
    catch (e) { notify('❌ ' + e.message, true); return; }
    const a1 = escAttr(artist), a2 = escAttr(album);
    let html = '<div class="album-header"><h2>💿 ' + escapeHtml(cleanMetaName(album) || album) + '</h2>' +
        '<div class="artist-stats">' + escapeHtml(artist) + '</div>' +
        '<div class="album-actions">' +
        '<button data-music-action="play-album" data-artist="' + a1 + '" data-album="' + a2 + '">▶️ Играть</button>' +
        '</div></div><div class="track-list">' +
        (d.tracks || []).map(renderTrackRow).join('') + '</div>';
    showMusicDetail(html);
}

async function playArtist(name) {
    try { const d = await (await apiFetch('/api/music/play/artist/' + encodeURIComponent(name), {method: 'POST'})).json(); notify('▶️ ' + (d.count || 0) + ' треков'); musicLoadStatus(); }
    catch (e) { notify('❌ ' + e.message, true); }
}
async function enqueueArtist(name) {
    try {
        const d = await (await apiFetch('/api/music/artist/' + encodeURIComponent(name))).json();
        await apiFetch('/api/music/queue/clear', {method: 'POST'});
        for (const t of (d.tracks || [])) {
            await apiFetch('/api/music/queue/add', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({file: t.file})});
        }
        notify('➕ ' + (d.tracks || []).length + ' треков в очереди');
    } catch (e) { notify('❌ ' + e.message, true); }
}
async function playAlbumBulk(artist, album) {
    try { await apiFetch('/api/music/play/album', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({artist, album})}); notify('▶️ Альбом играет'); musicLoadStatus(); }
    catch (e) { notify('❌ ' + e.message, true); }
}
async function playSavedPlaylistSidebar(name) {
    try { await apiFetch('/api/music/saved-playlists/' + encodeURIComponent(name) + '/play', {method: 'POST'}); notify('▶️ Плейлист: ' + name); musicLoadStatus(); }
    catch (e) { notify('❌ ' + e.message, true); }
}

async function registerPush() {
    try {
        if (!('Notification' in window)) return;
        if (Notification.permission === 'default') {
            await Notification.requestPermission();
        }
        if (Notification.permission === 'granted') {
            console.log('[push] notifications enabled');
        }
    } catch (e) {}
}


// ==================== МУЗЫКА ====================
function fmtMusicTime(s) {
    if (!s || s < 0) return '0:00';
    const m = Math.floor(s / 60), sec = Math.floor(s % 60);
    return m + ':' + String(sec).padStart(2, '0');
}

let musicDuration = 0;
let currentPlayingFile = null;
let musicCurrentVolume = 100;

function updateMusicProgress(elapsed, duration) {
    musicDuration = duration;
    const pct = duration > 0 ? (elapsed / duration) * 100 : 0;
    document.getElementById('music-seek-fill').style.width = pct + '%';
    document.getElementById('music-seek-handle').style.left = pct + '%';
    document.getElementById('music-elapsed').textContent = fmtMusicTime(elapsed);
    document.getElementById('music-duration').textContent = fmtMusicTime(duration);
}

async function musicSeek(sec) {
    try {
        await fetch('/api/music/seek', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({position: Math.floor(sec)})});
        musicLoadStatus();
    } catch (e) {}
}

function updateAudioBadgeFrom(ai) {
    const b = document.getElementById('audio-badge');
    if (!b) return;
    if (localStorage.getItem('bit-perfect-enabled') === 'false') {
        b.style.display = 'none';
        return;
    }
    b.className = 'audio-badge';
    if (!ai || (!ai.sample_rate && !ai.dsd && !ai.bit_perfect && !ai.resampled)) {
        b.style.display = 'none';
        return;
    }
    b.style.display = '';
    if (ai.dsd) {
        let tag = 'DSD';
        const sr = ai.sample_rate || ai.source_rate || 0;
        if (sr >= 5644800) tag = 'DSD128';
        else if (sr >= 2822400) tag = 'DSD64';
        b.textContent = tag; b.classList.add('dsd');
    } else if (ai.bit_perfect) {
        b.textContent = 'BIT-PERFECT'; b.classList.add('bit-perfect');
    } else if (ai.resampled) {
        b.textContent = 'RESAMPLED'; b.classList.add('resampled');
    } else {
        b.textContent = 'PCM'; b.classList.add('pcm');
    }
}

async function musicLoadStatus() {
    try {
        const r = await fetch('/api/music/status');
        const d = await r.json();
        const si = d.song_info || {};
        document.getElementById('music-title').textContent = si.Title || si.title || '—';
        document.getElementById('music-artist').textContent = si.Artist || si.artist || '—';
        document.getElementById('music-album').textContent = cleanMetaName(si.Album || si.album) || '—';
        document.getElementById('music-audio-info').textContent = (d.audio && d.audio.label) ? d.audio.label : '';
        updateAudioBadgeFrom(d.audio || {});
        document.getElementById('music-play-btn').textContent = d.state === 'play' ? '⏸' : '▶️';
        currentPlayingFile = si.file || null;
        if (si.file && (si.Artist || si.artist) && (si.Album || si.album)) {
            const a = si.Artist || si.artist, al = si.Album || si.album;
            const cover = document.getElementById('music-cover');
            const src = '/api/music/covers/' + encodeURIComponent(a) + '/' + encodeURIComponent(al) + '?v=v6';
            if (cover.getAttribute('src') !== src) { cover.style.display = ''; cover.src = src; }
        }
        const vol = document.getElementById('music-volume');
        if (d.volume !== undefined && document.activeElement !== vol) vol.value = d.volume;
        if (d.volume !== undefined) musicCurrentVolume = d.volume;

        updateMusicProgress(d.elapsed || 0, d.duration || 0);

        const sb = document.getElementById('music-shuffle-btn');
        if (sb) sb.style.color = d.random ? 'var(--accent)' : 'var(--fg)';
        const rb = document.getElementById('music-repeat-btn');
        if (rb) rb.style.color = d.repeat ? 'var(--accent)' : 'var(--fg)';

        const viz = document.getElementById('music-visualizer');
        if (d.state === 'play') { viz.classList.remove('paused'); startVisualizer(); }
        else { viz.classList.add('paused'); stopVisualizer(); }

        if (si.Title || si.title) document.title = '▶️ ' + (si.Title || si.title) + ' — NAS';
        // Глобальное состояние для плеер-бара/расширенного плеера
        window._cur = {
            artist: si.Artist || si.artist || '',
            album: si.Album || si.album || '',
            title: si.Title || si.title || si.file || '—',
            file: si.file || '',
            duration: d.duration || 0,
            elapsed: d.elapsed || 0,
            playing: d.state === 'play'
        };
        updateFullPlayer();
        if ('mediaSession' in navigator) {
            try {
                navigator.mediaSession.metadata = new MediaMetadata({
                    title: si.Title || si.title || '—',
                    artist: si.Artist || si.artist || '—',
                    album: si.Album || si.album || '—',
                    artwork: [{src: (si.Artist || si.artist) && (si.Album || si.album) ? '/api/music/covers/' + encodeURIComponent(si.Artist || si.artist) + '/' + encodeURIComponent(si.Album || si.album) + '?v=v6' : '', sizes: '512x512'}]
                });
            } catch (e) {}
        }
    } catch (e) {}
}

function musicCmd(cmd) {
    fetch('/api/music/' + cmd, {method: 'POST'}).then(() => musicLoadStatus());
}
function musicPlayPause() { musicCmd('pause'); }
function musicNext() { musicCmd('next'); }
function musicPrev() { musicCmd('prev'); }

// ---- Расширенный плеер ----
function updateFullPlayer() {
    const c = window._cur;
    if (!c) return;
    const set = (id, v) => { const el = document.getElementById(id); if (el) el.textContent = v; };
    set('fp-title', c.title || '—');
    set('fp-artist', c.artist || '—');
    set('fp-album', cleanMetaName(c.album) || '');
    const fp = document.getElementById('fp-cover');
    if (fp) {
        if (c.artist && c.album) { fp.style.visibility = ''; fp.src = '/api/music/covers/' + encodeURIComponent(c.artist) + '/' + encodeURIComponent(c.album) + '?v=v6'; }
        else { fp.style.visibility = 'hidden'; }
    }
    const play = document.getElementById('fp-play');
    if (play) play.textContent = c.playing ? '⏸' : '▶️';
    const fpv = document.getElementById('fp-volume');
    if (fpv && document.activeElement !== fpv) fpv.value = musicCurrentVolume;
    // прогресс (не трогаем, если пользователь тянет)
    if (!window._seeking) {
        const el = document.getElementById('fp-elapsed');
        const du = document.getElementById('fp-duration');
        if (el) el.textContent = fmtMusicTime(c.elapsed || 0);
        if (du) du.textContent = fmtMusicTime(c.duration || 0);
        const sk = document.getElementById('fp-seek');
        if (sk && document.activeElement !== sk && c.duration > 0) sk.value = Math.round((c.elapsed / c.duration) * 1000);
    }
}
function openFullPlayer() {
    const m = document.getElementById('full-player-modal');
    if (!m) return;
    m.classList.add('show');
    updateFullPlayer();
}
function closeFullPlayer() {
    const m = document.getElementById('full-player-modal');
    if (m) m.classList.remove('show');
}
// Мини-плеер в шапке доступен с любой вкладки — сначала переключаем на «Музыку».
function openCurrentAlbum() {
    const c = window._cur;
    if (c && c.artist && c.album) {
        const m = document.getElementById('full-player-modal'); if (m) m.classList.remove('show');
        showTab('music');
        openAlbum(c.artist, c.album);
    } else { notify('Нет данных об альбоме', true); }
}
function openCurrentArtist() {
    const c = window._cur;
    if (c && c.artist) {
        const m = document.getElementById('full-player-modal'); if (m) m.classList.remove('show');
        showTab('music');
        openArtist(c.artist);
    } else { notify('Нет данных об артисте', true); }
}
function scrollToCurrentTrack() {
    showTab('music');
    setTimeout(() => {
        const el = document.querySelector('.track-row.playing') || document.querySelector('.listen-row.playing');
        if (el) { el.scrollIntoView({behavior: 'smooth', block: 'center'}); el.classList.add('highlight'); setTimeout(() => el.classList.remove('highlight'), 2000); }
        else notify('Откройте список треков, чтобы найти текущий');
    }, 200);
}
// seek в расширенном плеере (range 0..1000)
function onSeekInput(value, isSlider) {
    window._seeking = true;
    const c = window._cur; if (!c || !c.duration) return;
    const el = document.getElementById('fp-elapsed');
    if (el) el.textContent = fmtMusicTime(c.duration * parseFloat(value) / 1000);
}
async function onSeekCommit(value) {
    const c = window._cur; if (!c || !c.duration) { window._seeking = false; return; }
    const pos = Math.floor(c.duration * parseFloat(value) / 1000);
    await musicSeek(pos);
    window._seeking = false;
}
function fpAddToPlaylist() { if (window._cur && window._cur.file) openAddToPlaylist(window._cur.file); }
async function fpAddToQueue() {
    if (!window._cur || !window._cur.file) return;
    try { await apiFetch('/api/music/queue/add', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({file: window._cur.file})}); notify('📋 Добавлено в очередь'); }
    catch (e) { notify('❌ ' + e.message, true); }
}
document.addEventListener('keydown', e => {
    const tag = (e.target && e.target.tagName) || '';
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    const inMusic = currentTab === 'music' || document.getElementById('full-player-modal').classList.contains('show');
    if (e.code === 'Space' && inMusic) { e.preventDefault(); musicPlayPause(); }
    else if (e.code === 'ArrowRight' && e.shiftKey && inMusic) { e.preventDefault(); musicNext(); }
    else if (e.code === 'ArrowLeft' && e.shiftKey && inMusic) { e.preventDefault(); musicPrev(); }
    else if (e.code === 'KeyF' && inMusic) { e.preventDefault(); openFullPlayer(); }
    else if (e.code === 'Escape') { closeFullPlayer(); }
});
async function musicShuffle() {
    try {
        const s = await (await fetch('/api/music/status')).json();
        await fetch('/api/music/random', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({enabled: !s.random})});
        musicLoadStatus();
    } catch (e) {}
}
async function musicRepeat() {
    try {
        const s = await (await fetch('/api/music/status')).json();
        await fetch('/api/music/repeat', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({enabled: !s.repeat})});
        musicLoadStatus();
    } catch (e) {}
}
function musicVolume(level) {
    fetch('/api/music/volume', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({level: parseInt(level)})}).then(() => musicLoadStatus());
}

async function musicLoadAlbums() {
    if (musicAudioMode === 'browser') { return loadBrowserAlbums(); }
    const grid = document.getElementById('music-albums');
    grid.innerHTML = skeletonGrid(12);
    try {
        const sort = document.getElementById('music-sort')?.value || 'artist';
        const r = await fetch('/api/music/albums?sort=' + sort);
        const d = await r.json();
        const albums = d.albums || [];
        document.getElementById('music-albums-count').textContent = '(' + albums.length + ')';
        renderMusicAlbums(albums);
    } catch (e) {
        grid.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

function musicTab(name, btn) {
    document.querySelectorAll('.music-tab').forEach(t => t.classList.remove('active'));
    btn.classList.add('active');
    document.querySelectorAll('[id^="music-content-"]').forEach(el => el.style.display = 'none');
    document.getElementById('music-content-' + name).style.display = '';
    if (name === 'playlists') musicLoadPlaylists();
    if (name === 'queue') musicLoadQueue();
    if (name === 'stats') loadMusicStats();
    if (name === 'smart') loadSmartPlaylist('recent');
    if (name === 'favorites') loadFavorites();
    if (name === 'recent') loadRecentMusic();
}

async function musicLoadPlaylists() {
    const grid = document.getElementById('music-playlists');
    const tracksBox = document.getElementById('playlist-tracks');
    if (tracksBox) tracksBox.innerHTML = '';
    grid.innerHTML = skeletonGrid(8);
    try {
        const r = await fetch('/api/music/saved-playlists');
        const d = await r.json();
        const list = d.playlists || [];
        if (!list.length) { grid.innerHTML = '<div class="loading">Нет плейлистов</div>'; return; }
        let html = '';
        list.forEach(p => {
            const safeName = escAttr(p.name);
            html += '<div class="music-album">';
            html += '<div class="music-album-cover" style="display:flex;align-items:center;justify-content:center;font-size:40px;cursor:pointer;" data-music-action="show-playlist" data-playlist="' + safeName + '">📋</div>';
            html += '<div class="music-album-title">' + escapeHtml(p.name) + '</div>';
            html += '<div style="display:flex;gap:4px;margin-top:6px;">';
            html += '<button data-music-action="play-playlist" data-playlist="' + safeName + '" style="flex:1;font-size:11px;">▶️ Играть</button>';
            html += '<button class="danger" data-music-action="delete-playlist" data-playlist="' + safeName + '" style="font-size:11px;">🗑️</button>';
            html += '</div></div>';
        });
        grid.innerHTML = html;
    } catch (e) { grid.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}

async function musicShowPlaylist(name) {
    const box = document.getElementById('playlist-tracks');
    if (!box) return;
    box.innerHTML = '<div class="loading">Загрузка...</div>';
    try {
        const r = await fetch('/api/music/saved-playlists/' + encodeURIComponent(name));
        const d = await r.json();
        const tracks = d.tracks || [];
        let html = '<h3 style="margin-top:16px;">📋 ' + escapeHtml(name) + ' <span style="color:var(--fg-dim);font-size:12px;">(' + tracks.length + ')</span></h3>';
        if (!tracks.length) { html += '<div class="loading">Пусто</div>'; box.innerHTML = html; return; }
        html += '<div class="album-tracks">';
        tracks.forEach((t, i) => {
            const file = t.file || '';
            html += '<div class="album-track" data-music-action="play-playlist-file" data-file="' + escAttr(file) + '">';
            html += '<span class="track-num">' + (i + 1) + '</span>';
            html += '<span class="track-title">' + escapeHtml(t.title || t.file || '') + '</span>';
            html += '<span class="track-duration">' + fmtMusicTime(t.duration || 0) + '</span>';
            html += '</div>';
        });
        html += '</div>';
        box.innerHTML = html;
    } catch (e) { box.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}

async function musicPlayPlaylistFile(file) {
    if (!file) return;
    try {
        await fetch('/api/music/play-file', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({file})});
        musicLoadStatus();
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function musicPlayPlaylist(name) {
    try {
        await fetch('/api/music/saved-playlists/' + encodeURIComponent(name) + '/play', { method: 'POST' });
        notify('▶️ Плейлист: ' + name);
        musicLoadStatus();
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function musicDeletePlaylist(name) {
    if (!confirm('Удалить плейлист «' + name + '»?')) return;
    try {
        await fetch('/api/music/saved-playlists/' + encodeURIComponent(name), { method: 'DELETE' });
        notify('🗑️ Удалён: ' + name);
        musicLoadPlaylists();
    } catch (e) { notify('❌ ' + e.message, true); }
}

function openPlaylistModal() {
    const inp = document.getElementById('playlist-name-input');
    if (inp) inp.value = '';
    document.getElementById('playlist-modal').classList.add('show');
    setTimeout(() => { if (inp) inp.focus(); }, 50);
}

function closePlaylistModal() {
    document.getElementById('playlist-modal').classList.remove('show');
}

async function createPlaylist() {
    const name = document.getElementById('playlist-name-input').value.trim();
    if (!name) return;
    try {
        const r = await fetch('/api/music/saved-playlists', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({name})});
        const d = await r.json();
        if (d.error) { notify('❌ ' + d.error, true); return; }
        closePlaylistModal();
        notify('💾 Плейлист создан: ' + name);
        musicLoadPlaylists();
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function musicLoadQueue() {
    const list = document.getElementById('music-queue');
    if (!list) return;
    try {
        const r = await fetch('/api/music/queue');
        const d = await r.json();
        const tracks = d.tracks || [];
        if (!tracks.length) { list.innerHTML = '<div class="loading">Очередь пуста</div>'; return; }
        let html = '';
        tracks.forEach((t, i) => {
            const id = t.id || t.pos || '';
            html += '<div class="album-track">';
            html += '<span class="track-num" data-music-action="queue-play" data-id="' + escAttr(id) + '" style="cursor:pointer;">' + (i + 1) + '</span>';
            html += '<span class="track-title" data-music-action="queue-play" data-id="' + escAttr(id) + '" style="cursor:pointer;flex:1;">' + escapeHtml(t.title || t.file || '') + '</span>';
            html += '<span class="track-duration">' + fmtMusicTime(t.duration || 0) + '</span>';
            html += '<span class="track-actions">' + addPlBtn(t.file) + '<button data-music-action="queue-delete" data-id="' + escAttr(id) + '" style="background:transparent;border:none;color:var(--error);cursor:pointer;font-size:14px;" title="Удалить">🗑️</button></span>';
            html += '</div>';
        });
        list.innerHTML = html;
        if (typeof initQueueDragAndDrop === 'function') initQueueDragAndDrop();
    } catch (e) { list.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}

async function musicQueuePlay(id) {
    if (!id) return;
    try {
        await fetch('/api/music/queue/play/' + id, { method: 'POST' });
        musicLoadStatus();
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function musicQueueDelete(id) {
    if (!id) return;
    await fetch('/api/music/queue/' + id, { method: 'DELETE' }).catch(() => {});
    musicLoadQueue();
}

async function musicQueueClear() {
    await fetch('/api/music/queue/clear', { method: 'POST' });
    musicLoadQueue();
}


// ==================== ➕ В ПЛЕЙЛИСТ ====================
let pendingTrackFile = null;
function addPlBtn(file) {
    if (!file) return '';
    return '<button class="track-add-pl" data-music-action="add-to-playlist" data-file="' + escAttr(file) + '" title="Добавить в плейлист">➕</button>';
}
async function openAddToPlaylist(file) {
    pendingTrackFile = file;
    document.getElementById('add-pl-current-track').textContent = '📁 ' + pendingTrackFile.split('/').pop();
    const listEl = document.getElementById('playlists-list');
    listEl.innerHTML = '<div class="loading">Загрузка...</div>';
    document.getElementById('add-to-playlist-modal').classList.add('show');
    try {
        const r = await apiFetch('/api/music/saved-playlists');
        const data = await r.json();
        let html = '';
        (data.playlists || []).forEach(p => {
            const name = p.name || p;
            html += '<div class="playlist-option" data-music-action="add-to-playlist-name" data-playlist="' + escAttr(name) + '">📋 ' + escapeHtml(name) + '</div>';
        });
        if (!html) html = '<div style="color:var(--fg-dim);">Нет плейлистов. Создайте первый выше.</div>';
        listEl.innerHTML = html;
    } catch (e) { listEl.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}
function closeAddToPlaylist() {
    document.getElementById('add-to-playlist-modal').classList.remove('show');
    pendingTrackFile = null;
}
async function addToPlaylist(name) {
    if (!pendingTrackFile) return;
    try {
        const r = await apiFetch('/api/music/saved-playlists/' + encodeURIComponent(name) + '/tracks', {
            method: 'POST', headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({file: pendingTrackFile})
        });
        if (r.ok) { notify('✅ Добавлено в ' + name); closeAddToPlaylist(); }
        else { notify('❌ Ошибка добавления', true); }
    } catch (e) { notify('❌ ' + e.message, true); }
}
async function createPlaylistAndAdd() {
    const name = document.getElementById('new-playlist-name').value.trim();
    if (!name) { notify('Введите имя плейлиста', true); return; }
    if (!pendingTrackFile) return;
    try {
        const r = await apiFetch('/api/music/saved-playlists', {
            method: 'POST', headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({name: name, files: [pendingTrackFile]})
        });
        if (r.ok) { notify('✅ Создан плейлист ' + name); closeAddToPlaylist(); }
        else { notify('❌ Ошибка создания', true); }
    } catch (e) { notify('❌ ' + e.message, true); }
}
document.addEventListener('keydown', e => {
    if (e.key === 'Escape') {
        const m = document.getElementById('add-to-playlist-modal');
        if (m && m.classList.contains('show')) closeAddToPlaylist();
    }
});

async function playTrack(file) {
    try {
        await fetch('/api/music/play-file', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({file})});
        musicLoadStatus();
    } catch (e) { notify('❌ Ошибка: ' + e.message, true); }
}

// Перемотка: клик и drag по прогресс-бару
(function initMusicSeek() {
    const bar = document.getElementById('music-seek-bar');
    if (!bar) return;
    let isDragging = false;
    function seekFromEvent(e) {
        const rect = bar.getBoundingClientRect();
        const cx = (e.clientX !== undefined ? e.clientX : (e.touches && e.touches[0] ? e.touches[0].clientX : 0));
        const x = cx - rect.left;
        const pct = Math.max(0, Math.min(1, x / rect.width));
        musicSeek(pct * musicDuration);
    }
    bar.addEventListener('mousedown', (e) => { isDragging = true; seekFromEvent(e); });
    bar.addEventListener('touchstart', (e) => { isDragging = true; seekFromEvent(e); });
    document.addEventListener('mousemove', (e) => { if (isDragging) seekFromEvent(e); });
    document.addEventListener('touchmove', (e) => { if (isDragging) seekFromEvent(e); });
    document.addEventListener('mouseup', () => isDragging = false);
    document.addEventListener('touchend', () => isDragging = false);
})();

if ('mediaSession' in navigator) {
    try {
        navigator.mediaSession.setActionHandler('play', () => musicPlayPause());
        navigator.mediaSession.setActionHandler('pause', () => musicPlayPause());
        navigator.mediaSession.setActionHandler('previoustrack', () => musicPrev());
        navigator.mediaSession.setActionHandler('nexttrack', () => musicNext());
    } catch (e) {}
}

function findMusicInTrackers() {
    const q = document.getElementById('music-search-input').value.trim();
    goToTab('torrents');
    const input = document.getElementById('torrent-query');
    if (input) { input.value = q || ''; input.focus(); }
    const sel = document.getElementById('torrent-category');
    if (sel) sel.value = '3000';
    if (q) searchTorrentsUI();
}

async function globalMusicPoll() {
    try {
        const r = await fetch('/api/music/status');
        const status = await r.json();
        const mp = document.getElementById('mini-player');
        const si = status.song_info || {};
        const artist = si.Artist || si.artist || '';
        const songTitle = si.Title || si.title || '';
        const album = si.Album || si.album || '';
        if (status.state === 'play' || status.state === 'pause') {
            mp.classList.add('active');
            const t = songTitle || si.file || '—';
            document.getElementById('mini-title').textContent = t;
            const ma = document.getElementById('mini-artist');
            if (ma) ma.textContent = artist || '—';
            document.title = (status.state === 'play' ? '▶️ ' : '⏸ ') + ((artist ? artist + ' — ' : '') + t) + ' — NAS';
        } else {
            mp.classList.remove('active');
            document.title = 'NAS Dashboard';
        }
        const dur = parseFloat(status.duration || 0);
        const el = parseFloat(status.elapsed || 0);
        const pct = dur > 0 ? Math.min(100, (el / dur) * 100) : 0;
        const fill = document.getElementById('mini-progress-fill');
        if (fill) fill.style.width = pct + '%';
        const vol = document.getElementById('mini-volume');
        if (vol) vol.textContent = (status.volume != null ? status.volume : '–');
        const vr = document.getElementById('mini-volume-range');
        if (vr && status.volume != null && document.activeElement !== vr) vr.value = status.volume;
        const ci = document.getElementById('mini-cover');
        if (ci && artist && album) {
            ci.src = '/api/music/covers/' + encodeURIComponent(artist) + '/' + encodeURIComponent(album) + '?v=v6';
            ci.style.display = '';
        }
        // Block 14: прогресс длинных треков (>10 мин)
        if (status.state === 'play' && dur > 600 && si.file) {
            const now = Math.floor(el);
            if (now > 0 && now % 30 < 5) {
                apiFetch('/api/music/track/progress', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({file_path: si.file, position_sec: now, duration_sec: Math.round(dur)})}).catch(()=>{});
            }
        }
    } catch (e) {}
}

async function musicVolumeUp() {
    try {
        const r = await apiFetch('/api/music/status');
        const d = await r.json();
        const v = Math.min(100, (d.volume || 75) + 5);
        await apiFetch('/api/music/volume', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({level: v})});
        globalMusicPoll();
    } catch (e) {}
}

async function musicVolumeDown() {
    try {
        const r = await apiFetch('/api/music/status');
        const d = await r.json();
        const v = Math.max(0, (d.volume || 75) - 5);
        await apiFetch('/api/music/volume', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({level: v})});
        globalMusicPoll();
    } catch (e) {}
}

async function musicMute() {
    try { await apiFetch('/api/music/volume', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({level: 0})}); globalMusicPoll(); } catch (e) {}
}

// ==================== ТЕКСТ ПЕСНИ (LRCLIB) ====================
async function musicShowLyrics() {
    const r = await apiFetch('/api/music/status');
    const s = await r.json();
    const si = s.song_info || {};
    const artist = si.Artist || si.artist || '';
    const title = si.Title || si.title || '';
    if (!artist || !title) { notify('Нет активного трека'); return; }
    document.getElementById('lyrics-title').textContent = artist + ' — ' + title;
    document.getElementById('lyrics-body').textContent = 'Загрузка...';
    document.getElementById('lyrics-modal').classList.add('show');
    try {
        const params = new URLSearchParams({artist, track: title, album: si.Album || si.album || '', duration: Math.round(s.duration || 0)});
        const lr = await apiFetch('/api/music/lyrics?' + params.toString());
        if (!lr.ok) { document.getElementById('lyrics-body').textContent = '📝 Текст не найден'; return; }
        const d = await lr.json();
        document.getElementById('lyrics-body').textContent = d.syncedLyrics || d.plainLyrics || '📝 Текст не найден';
    } catch (e) { document.getElementById('lyrics-body').textContent = 'Ошибка: ' + e.message; }
}
function closeLyrics() { document.getElementById('lyrics-modal').classList.remove('show'); }

// ==================== ПОХОЖИЕ ТРЕКИ ====================
async function musicShowSimilar() {
    const r = await apiFetch('/api/music/status');
    const s = await r.json();
    const si = s.song_info || {};
    const artist = si.Artist || si.artist || '';
    const file = si.file || '';
    if (!artist) { notify('Нет активного трека'); return; }
    document.getElementById('similar-modal').classList.add('show');
    document.getElementById('similar-body').innerHTML = '<div class="loading">Загрузка...</div>';
    try {
        const rr = await apiFetch('/api/music/similar?artist=' + encodeURIComponent(artist) + '&file=' + encodeURIComponent(file));
        const d = await rr.json();
        const tracks = d.tracks || [];
        if (!tracks.length) { document.getElementById('similar-body').innerHTML = '<div class="loading">Нет похожих</div>'; return; }
        let html = '';
        tracks.forEach(t => {
            const f = t.file || '';
            html += '<div class="album-track" data-music-action="play-file" data-file="' + escAttr(f) + '"><span class="track-title">' + escapeHtml(t.title || t.file || '') + '</span><span class="track-duration">' + escapeHtml(t.artist || '') + '</span><span class="track-actions">' + addPlBtn(t.file) + '</span></div>';
        });
        document.getElementById('similar-body').innerHTML = html;
    } catch (e) { document.getElementById('similar-body').innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}
function closeSimilar() { document.getElementById('similar-modal').classList.remove('show'); }

async function musicPlayFile(file) {
    if (!file) return;
    try {
        await apiFetch('/api/music/play-file', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({file})});
        closeSimilar();
        musicLoadStatus();
    } catch (e) { notify('❌ ' + e.message, true); }
}

// ==================== ИЗБРАННОЕ ====================
async function toggleFavorite() {
    try {
        const r = await apiFetch('/api/music/status');
        const s = await r.json();
        const si = s.song_info || {};
        if (!si.file) { notify('Нет активного трека'); return; }
        const check = await apiFetch('/api/music/favorites/check?file_path=' + encodeURIComponent(si.file));
        const c = await check.json();
        if (c.favorite) {
            await apiFetch('/api/music/favorites/' + encodeURIComponent(si.file), {method: 'DELETE'});
            setFavIcon(false); notify('🤍 Убрано из избранного');
        } else {
            await apiFetch('/api/music/favorites', {method: 'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({file_path: si.file, title: si.Title || si.title || '', artist: si.Artist || si.artist || '', album: si.Album || si.album || ''})});
            setFavIcon(true); notify('❤️ Добавлено в избранное');
        }
    } catch (e) { notify('❌ ' + e.message, true); }
}
function setFavIcon(on) { const b = document.getElementById('fav-btn'); if (b) b.textContent = on ? '❤️' : '🤍'; }

async function loadFavorites() {
    const el = document.getElementById('music-favorites');
    if (!el) return;
    el.innerHTML = '<div class="loading">Загрузка...</div>';
    try {
        const r = await apiFetch('/api/music/favorites');
        const d = await r.json();
        const items = d.favorites || [];
        if (!items.length) { el.innerHTML = '<div class="loading">Нет избранного</div>'; return; }
        let html = '<div class="album-tracks">';
        items.forEach(f => {
            const fp = f.file_path || '';
            html += '<div class="album-track" data-music-action="play-file" data-file="' + escAttr(fp) + '"><span class="track-title">' + escapeHtml((f.artist||'') + ' — ' + (f.title||'')) + '</span><span class="track-actions">' + addPlBtn(f.file_path) + '<button data-music-action="remove-favorite" data-file="' + escAttr(fp) + '" style="background:transparent;border:none;color:var(--error);cursor:pointer;">🗑️</button></span></div>';
        });
        html += '</div>';
        el.innerHTML = html;
    } catch (e) { el.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}
async function removeFavorite(fp) { await apiFetch('/api/music/favorites/' + encodeURIComponent(fp), {method:'DELETE'}); loadFavorites(); }

// ==================== СЛУШАЛИ НЕДАВНО ====================
async function loadRecentMusic() {
    const el = document.getElementById('music-recent');
    if (!el) return;
    el.innerHTML = '<div class="loading">Загрузка...</div>';
    refreshListensCount();
    try {
        const r = await apiFetch('/api/history/events?type=music.played&limit=80');
        const d = await r.json();
        const events = d.events || [];
        const musicExts = ['flac','mp3','m4a','ogg','opus','wav','aiff','ape','dsf','dff','wv','tta'];
        let html = '';
        let shown = 0;
        events.forEach(e => {
            let p = {};
            try { p = JSON.parse(e.payload || '{}'); } catch (err) { p = {}; }
            const file = p.file || p.track_path || '';
            if (!file) return;
            const ext = (file.split('.').pop() || '').toLowerCase();
            if (!musicExts.includes(ext)) return;                 // только музыка
            const base = (file.split('/').pop() || '');
            const low = base.toLowerCase();
            if (low.includes('test') || low.includes('sample')) return;
            const title = p.title || base.replace(/\.[^.]+$/, '') || 'Без названия';
            const artist = p.artist || 'Неизвестный';
            const album = p.album || '';
            html += '<div class="listen-row" data-music-action="play-file" data-file="' + escAttr(file) + '">' +
                '<span class="listen-artist">' + escapeHtml(artist) + '</span>' +
                '<span class="listen-title">' + escapeHtml(title) + '</span>' +
                (album ? '<span class="listen-album">' + escapeHtml(album) + '</span>' : '') +
                '<span class="listen-time">' + new Date(e.created_at).toLocaleString('ru-RU') + '</span>' +
                '<span class="track-actions">' + addPlBtn(file) + '</span>' +
                '</div>';
            shown++;
        });
        if (!shown) { el.innerHTML = '<div class="loading">Пока пусто</div>'; return; }
        el.innerHTML = '<div style="font-size:12px;color:var(--fg-dim);margin-bottom:6px;">Показано: ' + shown + '</div>' + html;
    } catch (e) { el.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>'; }
}

async function refreshListensCount() {
    try {
        const r = await apiFetch('/api/music/listens?limit=1');
        const d = await r.json();
        const el = document.getElementById('listens-count');
        if (el) el.textContent = (d.count !== undefined ? d.count : '—');
    } catch (e) {}
}

async function clearListens() {
    if (!confirm('Очистить историю прослушиваний?')) return;
    try {
        const r = await adminFetch('/api/music/listens?confirm=yes', {method: 'DELETE'});
        if (r.ok) { notify('🗑 История очищена'); loadRecentMusic(); }
        else notify('❌ Не удалось очистить (нужен ADMIN_TOKEN)', true);
    } catch (e) { notify('❌ ' + e.message, true); }
}

// ==================== РЕЙТИНГ ====================
async function setRating(n) {
    try {
        const r = await apiFetch('/api/music/status');
        const s = await r.json();
        const si = s.song_info || {};
        if (!si.file) return;
        await apiFetch('/api/music/ratings', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({file_path: si.file, rating: n})});
        renderRatingStars(n);
        notify('⭐ Рейтинг: ' + n);
    } catch (e) {}
}
function renderRatingStars(n) {
    const el = document.getElementById('rating-stars');
    if (!el) return;
    el.innerHTML = [1,2,3,4,5].map(i => '<span onclick="setRating(' + i + ')" style="cursor:pointer;color:' + (i <= n ? 'var(--warning)' : 'var(--fg-dim)') + ';">★</span>').join('');
}

// ==================== СПЯЩИЙ РЕЖИМ ====================
let sleepTimer = null;
function setSleepTimer(minutes) {
    if (sleepTimer) { clearTimeout(sleepTimer); sleepTimer = null; }
    if (minutes === 0) { notify('⏰ Таймер отменён'); return; }
    sleepTimer = setTimeout(() => { musicPlayPause(); notify('😴 Спящий режим: пауза'); }, minutes * 60 * 1000);
    notify('⏰ Таймер: ' + minutes + ' мин');
}

// ==================== ОЧЕРЕДЬ: DRAG & DROP ====================
function initQueueDragAndDrop() {
    const items = document.querySelectorAll('#music-queue .album-track');
    let dragged = null;
    items.forEach((item, idx) => {
        item.draggable = true;
        item.dataset.idx = idx;
        item.addEventListener('dragstart', () => { dragged = item; item.classList.add('dragging'); });
        item.addEventListener('dragover', (e) => e.preventDefault());
        item.addEventListener('dragend', () => {
            item.classList.remove('dragging');
            if (dragged) {
                const from = parseInt(dragged.dataset.idx);
                const to = parseInt(item.dataset.idx);
                if (from !== to) {
                    apiFetch('/api/music/queue/move', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({from, to})}).then(() => musicLoadQueue());
                }
            }
            dragged = null;
        });
    });
}

// ==================== АРТИСТ / RUTRACKER (альбом) ====================
async function playArtistOnly() {
    if (!currentAlbum) return;
    try {
        await apiFetch('/api/music/play/artist', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({artist: currentAlbum.artist})});
        notify('🎤 Только: ' + currentAlbum.artist);
        closeAlbumModal();
        musicLoadStatus();
    } catch (e) { notify('❌ ' + e.message, true); }
}

function searchAlbumOnRuTracker() {
    if (!currentAlbum) return;
    const query = currentAlbum.artist + ' ' + currentAlbum.album + ' flac';
    closeAlbumModal();
    showTab('torrents');
    setTimeout(() => {
        const input = document.getElementById('torrent-query');
        if (input) { input.value = query; }
        if (typeof searchTorrentsUI === 'function') searchTorrentsUI();
    }, 200);
}

async function loadDsSettings() {
    const sel = document.getElementById('ds-max-download');
    const st = document.getElementById('ds-status');
    if (!sel) return;
    try {
        const r = await apiFetch('/api/torrent/settings/download-station');
        const d = await r.json();
        if (!r.ok || d.error) {
            if (st) { st.textContent = '⚠️ ' + (d.error || 'недоступно'); st.className = 'error'; }
            return;
        }
        const val = parseInt(d.max_download, 10) || 0;
        ensureDsOption(sel, val);
        sel.value = String(val);
        if (st) { st.textContent = val === 0 ? 'текущий: без лимита' : ''; st.className = ''; }
    } catch (e) {
        if (st) { st.textContent = '⚠️ ' + e.message; st.className = 'error'; }
    }
}

function ensureDsOption(sel, val) {
    if (val <= 0) return;
    if (!Array.from(sel.options).some(o => parseInt(o.value, 10) === val)) {
        const o = document.createElement('option');
        o.value = String(val);
        o.textContent = String(val);
        sel.appendChild(o);
    }
}

async function saveDsSettings() {
    const sel = document.getElementById('ds-max-download');
    const st = document.getElementById('ds-status');
    if (!sel) return;
    const n = parseInt(sel.value, 10);
    if (isNaN(n) || n < 0) return;
    if (st) { st.textContent = 'Сохранение...'; st.className = 'saving'; }
    try {
        const r = await apiFetch('/api/torrent/settings/download-station', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({max_download: n})
        });
        const d = await r.json().catch(() => ({}));
        if (r.ok && !d.error) {
            if (st) { st.textContent = '✅ Сохранено: ' + n; st.className = ''; }
            notify('⚙️ Лимит загрузок: ' + n);
        } else {
            if (st) { st.textContent = '❌ ' + (d.error || 'ошибка'); st.className = 'error'; }
            notify('❌ ' + (d.error || 'ошибка'), true);
        }
    } catch (e) {
        if (st) { st.textContent = '❌ ' + e.message; st.className = 'error'; }
    }
}

async function loadTorrentStats() {
    try {
        const r = await fetch('/api/torrent/stats');
        const d = await r.json();
        document.getElementById('ts-active').textContent = d.active || 0;
        document.getElementById('ts-seeding').textContent = d.seeding || 0;
        document.getElementById('ts-finished').textContent = d.finished || 0;
        document.getElementById('ts-error').textContent = d.error || 0;
        document.getElementById('ts-downloaded').textContent = ((d.downloaded || 0) / 1073741824).toFixed(1);
        document.getElementById('ts-uploaded').textContent = ((d.uploaded || 0) / 1073741824).toFixed(1);
        document.getElementById('ts-ratio').textContent = (d.ratio || 0).toFixed(2);
    } catch (e) {}
}

async function loadRutrackerProfile() {
    try {
        const r = await fetch('/api/torrent/rutracker/profile');
        const d = await r.json();
        const el = document.getElementById('rutracker-stats');
        if (d.error) {
            el.innerHTML = '<span class="rutracker-error">⚠️ ' + escapeHtml(d.error) + '</span>' +
                ' <button onclick="showRtHelp()" style="margin-left:8px;background:var(--bg-button);color:var(--fg);border:none;border-radius:6px;padding:4px 10px;font-size:12px;cursor:pointer;">📋 Как обновить</button>';
            return;
        }
        document.getElementById('rutracker-username').textContent = d.username || '—';
        if (d.stats_disabled) {
            el.innerHTML =
                '<span class="stat">👤 <b>' + escapeHtml(d.username || '') + '</b></span>' +
                '<span class="stat" style="color:var(--warning);">📊 Статистика RuTracker отключена в профиле</span>' +
                '<span class="stat" style="color:var(--fg-dim);">Включи её в настройках профиля на rutracker.org</span>';
            return;
        }
        const ratioClass = (d.ratio || 0) >= 1 ? 'good' : (d.ratio || 0) >= 0.5 ? 'warn' : 'bad';
        el.innerHTML =
            '<span class="stat">📊 Ratio: <b class="ratio-' + ratioClass + '">' + (d.ratio || 0).toFixed(2) + '</b></span>' +
            '<span class="stat">⬆️ ' + (d.uploaded_gb || 0).toFixed(2) + ' GB</span>' +
            '<span class="stat">⬇️ ' + (d.downloaded_gb || 0).toFixed(2) + ' GB</span>' +
            (d.bonus ? '<span class="stat">💰 ' + d.bonus + '</span>' : '') +
            (d.cached ? '<span class="stat" style="opacity:0.5;">(кэш)</span>' : '');
    } catch (e) {
        document.getElementById('rutracker-stats').innerHTML =
            '<span class="rutracker-error">⚠️ ' + escapeHtml(e.message) + '</span>' +
            ' <button onclick="showRtHelp()" style="margin-left:8px;background:var(--bg-button);color:var(--fg);border:none;border-radius:6px;padding:4px 10px;font-size:12px;cursor:pointer;">📋 Как обновить</button>';
    }
}

function showRtHelp() {
    alert(
        'Как обновить cookie RuTracker:\n\n' +
        '1. Открой https://rutracker.org в браузере (залогинен)\n' +
        '2. F12 → Application → Cookies → https://rutracker.org\n' +
        '3. Скопируй bb_session и bb_data\n' +
        '4. В терминале выполни: rt-cookie\n' +
        '5. Вставь bb_session, затем bb_data\n' +
        '6. Перезагрузи страницу\n\n' +
        'Статистику профиля (ratio) можно включить в настройках профиля на rutracker.org.'
    );
}

async function refreshRutracker() {
    document.getElementById('rutracker-stats').innerHTML = '<span>Обновление...</span>';
    await fetch('/api/torrent/rutracker/profile?refresh=1');
    loadRutrackerProfile();
}
