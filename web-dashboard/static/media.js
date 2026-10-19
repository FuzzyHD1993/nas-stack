// NAS Dashboard — media library, photos, player, cinema.
// Classic script: expects shared globals from app.js.

// ==================== МЕДИА ====================
let mediaSearchTimer = null;

function mediaFilterParams() {
    const p = new URLSearchParams();
    const q = document.getElementById('media-search').value.trim();
    const genre = document.getElementById('media-genre').value;
    const ymin = document.getElementById('media-year-min').value;
    const ymax = document.getElementById('media-year-max').value;
    const sort = document.getElementById('media-sort').value;
    if (q) p.set('q', q);
    if (genre) p.set('genre', genre);
    if (ymin) p.set('year_min', ymin);
    if (ymax) p.set('year_max', ymax);
    if (sort && sort !== 'title') p.set('sort', sort);
    return p.toString();
}

async function loadMedia() {
    const grid = document.getElementById('media-grid');
    grid.innerHTML = skeletonGrid(12);
    try {
        const qs = mediaFilterParams();
        const r = await fetch('/api/media/' + (mediaType === 'tv' ? 'tv' : 'movies') + (qs ? '?' + qs : ''));
        const d = await r.json();
        mediaItems = d.items || [];
        renderMedia();
        loadMediaStats();
    } catch (e) {
        grid.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

function onMediaSearch() {
    clearTimeout(mediaSearchTimer);
    mediaSearchTimer = setTimeout(loadMedia, 300);
}

function resetMediaFilters() {
    document.getElementById('media-search').value = '';
    document.getElementById('media-genre').value = '';
    document.getElementById('media-year-min').value = '';
    document.getElementById('media-year-max').value = '';
    document.getElementById('media-sort').value = 'title';
    loadMedia();
}

async function loadLibraryStats() {
    const el = document.getElementById('library-stats');
    try {
        const r = await fetch('/api/media/library/stats');
        const d = await r.json();
        const m = d.movies || {};
        const t = d.tv || {};
        const mg = m.by_genre || {};
        const genres = Object.entries(mg).sort((a, b) => b[1] - a[1]).slice(0, 5);
        let html = '';
        html += '<span class="search-count" style="margin-bottom:0;">🎬 Фильмы: ' + (m.total || 0) + ' (постеры: ' + (m.with_poster || 0) + ')</span>';
        html += '<span class="search-count" style="margin-bottom:0;">📺 Сериалы: ' + (t.total || 0) + ' · эпизодов: ' + (t.episodes || 0) + '</span>';
        if (genres.length > 0) {
            html += '<span class="search-count" style="margin-bottom:0;">Топ жанры: ' + genres.map(g => g[0]).join(', ') + '</span>';
        }
        el.innerHTML = html;
    } catch (e) {
        el.innerHTML = '';
    }
}

async function loadMediaFilters() {
    try {
        const [gr, yr] = await Promise.all([fetch('/api/media/genres'), fetch('/api/media/years')]);
        const gd = await gr.json();
        const yd = await yr.json();
        const genreSel = document.getElementById('media-genre');
        (gd.genres || []).forEach(genre => {
            const o = document.createElement('option');
            o.value = genre;
            o.textContent = genre;
            genreSel.appendChild(o);
        });
        const yearMin = document.getElementById('media-year-min');
        const yearMax = document.getElementById('media-year-max');
        (yd.years || []).forEach(year => {
            const o1 = document.createElement('option');
            o1.value = year; o1.textContent = year; yearMin.appendChild(o1);
            const o2 = document.createElement('option');
            o2.value = year; o2.textContent = year; yearMax.appendChild(o2);
        });
    } catch (e) {}
}

function renderMedia() {
    const grid = document.getElementById('media-grid');
    if (mediaItems.length === 0) {
        grid.innerHTML = '<div class="loading">Ничего не найдено. Нажмите «Обновить библиотеку».</div>';
        return;
    }
    let html = '';
    mediaItems.forEach(m => {
        const poster = m.poster_url
            ? '<img class="media-poster" src="/api/media/poster?url=' + encodeURIComponent(m.poster_url) + '" alt="" loading="lazy" onerror="this.outerHTML=\'<div class=&quot;media-poster-placeholder&quot;>🎬</div>\'" />'
            : '<div class="media-poster-placeholder">🎬</div>';
        html += '<div class="media-card" onclick="showMediaDetails(' + m.id + ')">';
        html += poster;
        html += '<div style="padding:8px 10px;">';
        html += '<div class="media-title">' + (m.title || 'Без названия') + '</div>';
        html += '<div class="media-meta">' + (m.year || '—') + (m.rating ? ' · ★ ' + Number(m.rating).toFixed(1) : '') + '</div>';
        html += '</div></div>';
    });
    grid.innerHTML = html;
}

function showMediaType(type, event) {
    mediaType = type;
    document.querySelectorAll('.media-tab').forEach(t => t.classList.remove('active'));
    event.target.classList.add('active');
    loadMedia();
}

async function scanMedia(event) {
    const btn = event.target;
    btn.disabled = true;
    btn.textContent = '🔄 Сканирование...';
    try {
        const r = await fetch('/api/media/scan', {method: 'POST'});
        const d = await r.json();
        if (d.status === 'already_running') {
            notify('📚 Сканирование уже идёт');
        } else {
            notify('📚 Сканирование запущено');
        }
        pollScanStatus(btn);
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
        btn.disabled = false;
        btn.textContent = '🔄 Обновить библиотеку';
    }
}

function pollScanStatus(btn) {
    if (mediaScanTimer) clearInterval(mediaScanTimer);
    mediaScanTimer = setInterval(async () => {
        try {
            const r = await fetch('/api/media/scan/status');
            const s = await r.json();
            if (s.running) {
                const total = s.total || 0;
                btn.textContent = total > 0 ? '🔄 Сканирую: ' + s.progress + '/' + total : '🔄 Сканирую...';
                document.getElementById('media-stats').textContent = 'Сканирование: ' + s.progress + '/' + total;
            } else {
                clearInterval(mediaScanTimer);
                mediaScanTimer = null;
                btn.disabled = false;
                btn.textContent = '🔄 Обновить библиотеку';
                const res = s.result || {};
                notify('📚 Готово: +' + (res.added || 0) + ' новых, обновлено ' + (res.updated || 0));
                loadMedia();
            }
        } catch (e) {
            clearInterval(mediaScanTimer);
            mediaScanTimer = null;
            btn.disabled = false;
            btn.textContent = '🔄 Обновить библиотеку';
        }
    }, 2000);
}

async function showMediaDetails(id) {
    currentMediaId = id;
    try {
        const r = await fetch('/api/media/' + (mediaType === 'tv' ? 'tv' : 'movies') + '/' + id);
        const m = await r.json();
        if (m.error) { notify('❌ ' + m.error, true); return; }
        const bc = document.getElementById('bc-current');
        if (bc) bc.textContent = (m.title || '—') + (m.year ? ' (' + m.year + ')' : '');
        if (mediaType === 'tv') {
            showSeriesModal(m);
            renderActorsInto('series-actors', 'series-actors-list', m.actors, m.director);
            loadSeasons(id);
        } else {
            document.getElementById('media-detail-title').textContent = (m.title || '') + (m.year ? ' (' + m.year + ')' : '');
            document.getElementById('media-detail-meta').textContent = (m.genres || '') + (m.rating ? ' · ★ ' + Number(m.rating).toFixed(1) : '') + (m.runtime ? ' · ' + m.runtime + ' мин' : '');
            document.getElementById('media-detail-overview').textContent = m.overview || '';
            document.getElementById('media-detail-poster').src = m.poster_url ? '/api/media/poster?url=' + encodeURIComponent(m.poster_url) : '';
            currentMediaTitle = m.title || '';
            currentMedia = m;
            document.getElementById('match-btn').style.display = (m.tmdb_id && m.poster_url) ? 'none' : 'inline-block';
            document.getElementById('trailer-btn').style.display = m.trailer_url ? 'inline-block' : 'none';
            document.getElementById('rename-btn').style.display = 'inline-block';
            document.getElementById('add-playlist-btn').style.display = 'inline-block';
            renderActorsInto('actors-block', 'actors-list', m.actors, m.director);
            document.getElementById('media-modal').classList.add('show');
            loadSimilar(id);
        }
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}

function renderActorsInto(blockId, listId, actors, director) {
    const block = document.getElementById(blockId);
    const list = document.getElementById(listId);
    if (!block || !list) return;
    actors = actors || [];
    if (actors.length === 0 && !director) { block.style.display = 'none'; return; }
    let html = '';
    if (director) {
        html += '<div class="director-line" onclick="openPersonModal(' + director.id + ')">🎬 Режиссёр: <b>' + escapeHtml(director.name) + '</b></div>';
    }
    actors.forEach(a => {
        const avatar = a.profile_url
            ? '<img src="/api/media/poster?url=' + encodeURIComponent(a.profile_url) + '" alt="" loading="lazy" onerror="this.outerHTML=\'<div class=&quot;actor-avatar-placeholder&quot;>👤</div>\'">'
            : '<div class="actor-avatar-placeholder">👤</div>';
        html += '<div class="actor-card" onclick="openPersonModal(' + a.id + ')">';
        html += avatar;
        html += '<div class="actor-name">' + escapeHtml(a.name) + '</div>';
        if (a.character) html += '<div class="actor-role">' + escapeHtml(a.character) + '</div>';
        html += '</div>';
    });
    list.innerHTML = html;
    block.style.display = '';
}

async function openPersonModal(personId) {
    if (!personId) return;
    const modal = document.getElementById('person-modal');
    document.getElementById('person-name').textContent = '…';
    document.getElementById('person-movies').innerHTML = skeletonGrid(8);
    const avatar = document.getElementById('person-avatar');
    avatar.style.display = 'none';
    modal.classList.add('show');
    try {
        const r = await fetch('/api/media/people/' + personId);
        const d = await r.json();
        if (d.error) { notify('❌ ' + d.error, true); closePersonModal(); return; }
        document.getElementById('person-name').textContent = d.name || '';
        if (d.profile_url) {
            avatar.src = '/api/media/poster?url=' + encodeURIComponent(d.profile_url);
            avatar.style.display = '';
            avatar.onerror = () => { avatar.style.display = 'none'; };
        }
        const movies = d.movies || [];
        const grid = document.getElementById('person-movies');
        if (movies.length === 0) { grid.innerHTML = '<div class="loading">Нет работ в библиотеке</div>'; return; }
        let html = '';
        movies.forEach(m => {
            const poster = m.poster_url
                ? '<img class="media-poster" src="/api/media/poster?url=' + encodeURIComponent(m.poster_url) + '" alt="" loading="lazy" onerror="this.outerHTML=\'<div class=&quot;media-poster-placeholder&quot;>🎬</div>\'">'
                : '<div class="media-poster-placeholder">🎬</div>';
            html += '<div class="media-card" onclick="closePersonModal(); openMediaFromPerson(' + m.id + ',\'' + m.media_type + '\')">';
            html += poster;
            html += '<div style="padding:8px 10px;"><div class="media-title">' + escapeHtml(m.title || '') + '</div>';
            html += '<div class="media-meta">' + (m.year || '—') + (m.rating ? ' · ★ ' + Number(m.rating).toFixed(1) : '') + '</div></div></div>';
        });
        grid.innerHTML = html;
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}

function openMediaFromPerson(id, type) {
    mediaType = type === 'tv' ? 'tv' : 'movie';
    showMediaDetails(id);
}

function closePersonModal() {
    document.getElementById('person-modal').classList.remove('show');
}

// ==================== ФОТО-ГАЛЕРЕЯ ====================
let photoItems = [];
let lightboxIndex = -1;

async function loadPhotos() {
    const container = document.getElementById('photos-groups');
    container.innerHTML = '<div class="photo-grid">' + skeletonGrid(12) + '</div>';
    try {
        const r = await fetch('/api/media/photos/groups');
        const d = await r.json();
        const groups = d.groups || [];
        photoItems = [];
        if (groups.length === 0) {
            container.innerHTML = '<div class="loading">Фотографии не найдены</div>';
            document.getElementById('photos-count').textContent = '';
            return;
        }
        let html = '';
        let total = 0;
        groups.forEach(g => {
            html += '<div class="photo-date-group"><h3>📅 ' + g.date + ' <span style="color:var(--fg-dim);font-size:12px;">(' + g.count + ')</span></h3>';
            html += '<div class="photo-grid">';
            (g.photos || []).forEach(p => {
                const idx = photoItems.length;
                photoItems.push(p);
                const tb = '/api/media/photos/thumbnail?path=' + encodeURIComponent(p.path);
                html += '<div class="photo-item" onclick="openLightbox(' + idx + ')">';
                html += '<img src="' + tb + '" loading="lazy" decoding="async" alt="" onerror="this.style.opacity=0.2">';
                if (p.type === 'video') html += '<span class="photo-badge">▶️</span>';
                html += '</div>';
            });
            html += '</div></div>';
            total += g.count;
        });
        container.innerHTML = html;
        document.getElementById('photos-count').textContent = '(' + total + ')';
    } catch (e) {
        container.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

function openLightbox(idx) {
    if (idx < 0 || idx >= photoItems.length) return;
    lightboxIndex = idx;
    const p = photoItems[idx];
    const img = document.getElementById('lightbox-img');
    const vid = document.getElementById('lightbox-video');
    const src = '/api/media/photos/full?path=' + encodeURIComponent(p.path);
    if (p.type === 'video') {
        img.style.display = 'none';
        vid.style.display = '';
        vid.src = src;
        vid.play().catch(() => {});
    } else {
        vid.style.display = 'none';
        vid.pause();
        vid.src = '';
        img.style.display = '';
        img.src = src;
    }
    document.getElementById('lightbox-caption').textContent = p.name + ' · ' + new Date((p.mod_time || 0) * 1000).toLocaleString('ru-RU');
    document.getElementById('photo-lightbox').classList.add('show');
}

function closeLightbox() {
    const vid = document.getElementById('lightbox-video');
    vid.pause();
    vid.src = '';
    document.getElementById('photo-lightbox').classList.remove('show');
}

function lightboxNav(dir) {
    if (photoItems.length === 0) return;
    if (!document.getElementById('photo-lightbox').classList.contains('show')) return;
    lightboxIndex = (lightboxIndex + dir + photoItems.length) % photoItems.length;
    openLightbox(lightboxIndex);
}

function showSeriesModal(m) {
    currentSeriesId = m.id;
    document.getElementById('series-title').textContent = (m.title || '') + (m.year ? ' (' + m.year + ')' : '');
    document.getElementById('series-meta').textContent = (m.genres || '') + (m.rating ? ' · ★ ' + Number(m.rating).toFixed(1) : '');
    document.getElementById('series-overview').textContent = m.overview || '';
    document.getElementById('series-poster').src = m.poster_url ? '/api/media/poster?url=' + encodeURIComponent(m.poster_url) : '';
    document.getElementById('series-modal').classList.add('show');
}

function closeSeriesModal() {
    document.getElementById('series-modal').classList.remove('show');
}

async function loadSeasons(seriesId) {
    currentSeriesId = seriesId;
    try {
        const r = await fetch('/api/media/tv/' + seriesId + '/seasons');
        const d = await r.json();
        const seasons = d.seasons || [];
        if (seasons.length > 0) {
            currentSeason = seasons[0].season;
            renderSeasonTabs(seasons);
            loadSeasonEpisodes(seriesId, currentSeason);
        } else {
            document.getElementById('season-tabs').innerHTML = '';
            document.getElementById('episode-list').innerHTML = '<div class="loading">Эпизоды не найдены</div>';
        }
    } catch (e) {
        document.getElementById('episode-list').innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

function renderSeasonTabs(seasons) {
    const tabs = document.getElementById('season-tabs');
    let html = '';
    seasons.forEach(s => {
        html += '<button class="season-tab' + (s.season === currentSeason ? ' active' : '') + '" data-season="' + s.season + '" onclick="selectSeason(' + s.season + ')">S' + String(s.season).padStart(2, '0') + ' (' + s.episode_count + ')</button>';
    });
    tabs.innerHTML = html;
}

function selectSeason(season) {
    currentSeason = season;
    document.querySelectorAll('.season-tab').forEach(t => {
        t.classList.toggle('active', parseInt(t.getAttribute('data-season'), 10) === season);
    });
    loadSeasonEpisodes(currentSeriesId, season);
}

async function loadSeasonEpisodes(seriesId, season) {
    const list = document.getElementById('episode-list');
    list.innerHTML = '<div class="loading">Загрузка эпизодов...</div>';
    try {
        const r = await fetch('/api/media/tv/' + seriesId + '/season/' + season);
        const d = await r.json();
        renderEpisodes(d.episodes || []);
    } catch (e) {
        list.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

async function renderEpisodes(episodes) {
    const list = document.getElementById('episode-list');
    if (episodes.length === 0) {
        list.innerHTML = '<div class="loading">Эпизоды не найдены</div>';
        return;
    }
    // Прогресс просмотра эпизодов (для прогресс-баров)
    const prog = {};
    try {
        const pr = await fetch('/api/media/progress/continue');
        const pd = await pr.json();
        (pd.items || []).forEach(it => {
            if (it.media_type === 'episode' && it.percent > 0) prog[it.media_id] = it.percent;
        });
    } catch (e) {}
    let html = '';
    episodes.forEach(e => {
        const thumb = e.thumbnail_url
            ? '<img class="episode-thumb" src="/api/media/poster?url=' + encodeURIComponent(e.thumbnail_url) + '" alt="" loading="lazy" onerror="this.style.display=\'none\'" />'
            : '<img class="episode-thumb" src="/api/media/episode/' + e.id + '/thumbnail" alt="" loading="lazy" onerror="this.style.display=\'none\'" />';
        const pct = prog[e.id] || 0;
        html += '<div class="episode-row" onclick="playEpisode(' + e.id + ')">';
        html += thumb;
        html += '<div class="episode-info">';
        html += '<div class="episode-number">S' + String(e.season).padStart(2, '0') + 'E' + String(e.episode).padStart(2, '0') + '</div>';
        html += '<div class="episode-title">' + (e.title || 'Эпизод ' + e.episode) + '</div>';
        if (e.overview) html += '<div class="episode-overview">' + e.overview + '</div>';
        if (pct > 0) html += '<div class="ep-progress" style="width:80px;height:4px;background:var(--bg-button);border-radius:2px;overflow:hidden;margin-top:4px;"><div style="width:' + pct + '%;height:100%;background:var(--accent);"></div></div>';
        html += '</div>';
        html += '<div class="episode-actions">';
        html += '<button onclick="event.stopPropagation(); playEpisode(' + e.id + ')" title="Смотреть">▶️</button>';
        html += '<button onclick="event.stopPropagation(); openEpisodeInVlc(' + e.id + ')" title="VLC">📺</button>';
        html += '<button onclick="event.stopPropagation(); copyEpisodePath(\'' + (e.path || '').replace(/'/g, "\\'") + '\')" title="Путь">📋</button>';
        html += '</div></div>';
    });
    list.innerHTML = html;
}

async function playEpisode(id) {
    if (!id) return;
    try {
        const r = await fetch('/api/media/episode/' + id);
        const e = await r.json();
        const base = 'http://' + window.location.hostname + ':8091';
        // Всегда transcoded — сервис сам решает copy/транскод видео
        const src = base + '/episode/' + id + '/stream/transcoded';
        const badge = document.getElementById('transcode-badge');
        const vc = (e.video_codec || '').toLowerCase();
        if (badge) badge.style.display = ['hevc', 'h265', 'av1', 'vp9'].includes(vc) ? '' : 'none';
        document.getElementById('series-modal').classList.remove('show');
        beginPlayback('episode', id, src);
    } catch (err) {
        notify('❌ Ошибка: ' + err.message, true);
    }
}

async function openEpisodeInVlc(id) {
    try {
        const r = await fetch('/api/media/episode/' + id + '/open', {method: 'POST'});
        const d = await r.json();
        if (d.error) notify('❌ ' + d.error, true);
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}

async function copyEpisodePath(path) {
    if (!path) { notify('❌ Путь не найден', true); return; }
    try {
        await navigator.clipboard.writeText(path);
        notify('📋 Путь скопирован');
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}

function closeMediaModal() {
    document.getElementById('media-modal').classList.remove('show');
}

function openTrailer() {
    if (currentMedia && currentMedia.trailer_url) {
        window.open(currentMedia.trailer_url, '_blank');
    }
}

async function renamePreview() {
    if (!currentMediaId) return;
    try {
        const r = await fetch('/api/media/rename/preview', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({id: currentMediaId})
        });
        const d = await r.json();
        if (d.error) { notify('❌ ' + d.error, true); return; }
        let msg = 'Текущий путь:\n' + d.current_path;
        msg += '\n\nПредложенный путь:\n' + d.suggested_path;
        if (d.exists) msg += '\n\n⚠️ Целевой путь уже существует!';
        if (d.episodes && d.episodes.length) msg += '\n\nЭпизодов: ' + d.episodes.length;
        if (!confirm(msg + '\n\nПрименить переименование?')) return;
        const ar = await fetch('/api/media/rename/apply', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({id: currentMediaId})
        });
        const ad = await ar.json();
        if (ad.error) { notify('❌ ' + ad.error, true); return; }
        notify('✏️ Переименовано');
        closeMediaModal();
        loadMedia();
    } catch (e) {
        notify('❌ ' + e.message, true);
    }
}

async function loadSimilar(mediaId) {
    const block = document.getElementById('similar-block');
    block.style.display = 'none';
    try {
        const r = await fetch('/api/media/movies/' + mediaId + '/similar');
        const d = await r.json();
        const items = d.items || [];
        if (items.length === 0) return;
        block.style.display = '';
        let html = '';
        items.forEach(m => {
            const poster = m.poster_url ? '/api/media/poster?url=' + encodeURIComponent(m.poster_url) : '';
            html += '<div class="similar-item" onclick="openMediaModal(' + m.id + ')">';
            html += poster ? '<img src="' + poster + '" loading="lazy">' : '<div class="similar-poster-placeholder">🎬</div>';
            html += '<div class="similar-title">' + escapeHtml(m.title || '') + '</div></div>';
        });
        document.getElementById('similar-grid').innerHTML = html;
    } catch (e) {}
}

function openMediaModal(id) {
    mediaType = 'movie';
    showMediaDetails(id);
}

async function loadSmartShelves() {
    const shelves = [
        { name: '⏯ Продолжить просмотр', url: '/api/media/progress/continue', kind: 'progress' },
        { name: '🆕 Недавно добавленные', url: '/api/media/smart/recent' },
        { name: '⭐ Топ рейтинг', url: '/api/media/smart/top_rated' },
        { name: '👁 Непросмотренные', url: '/api/media/smart/unwatched' }
    ];
    let html = '';
    for (const s of shelves) {
        try {
            const r = await fetch(s.url);
            const d = await r.json();
            const items = s.kind === 'progress' ? (d.items || []).map(it => ({ id: it.media_id, poster_url: null, kind: it.media_type })) : (d.items || []);
            if (items.length === 0) continue;
            html += '<div class="shelf"><h3 class="shelf-title">' + s.name + '</h3><div class="shelf-row">';
            items.slice(0, 15).forEach(m => {
                const poster = m.poster_url ? '/api/media/poster?url=' + encodeURIComponent(m.poster_url) : '';
                html += '<div class="shelf-item" onclick="openMediaModal(' + m.id + ')">';
                html += poster ? '<img src="' + poster + '" loading="lazy">' : '<div class="shelf-poster-placeholder">🎬</div>';
                html += '</div>';
            });
            html += '</div></div>';
        } catch (e) {}
    }
    document.getElementById('smart-shelves').innerHTML = html;
}

// ==================== TMDb МАТЧИНГ ====================
function openMatchModal() {
    document.getElementById('match-query').value = currentMediaTitle || '';
    document.getElementById('match-results').innerHTML = '';
    document.getElementById('media-modal').classList.remove('show');
    document.getElementById('match-modal').classList.add('show');
    if (currentMediaTitle) searchTMDBMatch();
}

function closeMatchModal() {
    document.getElementById('match-modal').classList.remove('show');
}

async function searchTMDBMatch() {
    const q = document.getElementById('match-query').value.trim();
    if (!q) return;
    const res = document.getElementById('match-results');
    res.innerHTML = '<div class="loading">Поиск...</div>';
    try {
        const type = mediaType === 'tv' ? 'tv' : 'movie';
        const r = await fetch('/api/media/match/search?q=' + encodeURIComponent(q) + '&type=' + type);
        const d = await r.json();
        const results = d.results || [];
        if (results.length === 0) {
            res.innerHTML = '<div class="loading">Ничего не найдено</div>';
            return;
        }
        let html = '';
        results.forEach(m => {
            const poster = m.poster_url ? '/api/media/poster?url=' + encodeURIComponent(m.poster_url) : '';
            html += '<div style="cursor:pointer; text-align:center;" onclick="matchMediaToTmdb(' + m.tmdb_id + ')">';
            html += poster ? '<img src="' + poster + '" style="width:100%; aspect-ratio:2/3; object-fit:cover; border-radius:6px;" />' : '<div style="width:100%;aspect-ratio:2/3;background:var(--bg-button);border-radius:6px;display:flex;align-items:center;justify-content:center;color:var(--fg-dim);">🎬</div>';
            html += '<div style="font-size:11px;color:var(--fg);margin-top:4px;">' + m.title + (m.year ? ' (' + m.year + ')' : '') + '</div>';
            html += '</div>';
        });
        res.innerHTML = html;
    } catch (e) {
        res.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

async function matchMediaToTmdb(tmdbId) {
    if (!currentMediaId) return;
    try {
        const r = await fetch('/api/media/match/' + currentMediaId, {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({tmdb_id: tmdbId})
        });
        const d = await r.json();
        if (d.error) { notify('❌ ' + d.error, true); return; }
        notify('✅ Сопоставлено с TMDb');
        closeMatchModal();
        loadMedia();
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}

async function openMediaItem() {
    if (!currentMediaId) return;
    try {
        const r = await fetch('/api/media/open/' + currentMediaId, {method: 'POST'});
        const d = await r.json();
        if (d.error) notify('❌ ' + d.error, true);
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}

async function openPlayer(id) {
    if (!id) return;
    try {
        const r = await fetch('/api/media/' + (mediaType === 'tv' ? 'tv' : 'movies') + '/' + id);
        const m = await r.json();
        const base = 'http://' + window.location.hostname + ':8091';
        // Всегда transcoded: сервис сам решает — copy видео или транскод HEVC→H.264
        const src = base + '/stream/transcoded/' + id;
        const badge = document.getElementById('transcode-badge');
        const vc = (m.video_codec || '').toLowerCase();
        if (badge) badge.style.display = ['hevc', 'h265', 'av1', 'vp9'].includes(vc) ? '' : 'none';
        beginPlayback('movie', id, src);
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}

function hideTranscodeBadge() {
    const badge = document.getElementById('transcode-badge');
    if (badge) badge.style.display = 'none';
}

function closePlayer() {
    const v = document.getElementById('player-video');
    if (document.pictureInPictureElement) { document.exitPictureInPicture().catch(() => {}); }
    v.pause();
    v.src = '';
    hideTranscodeBadge();
    document.getElementById('player-modal').classList.remove('show');
}

async function togglePiP() {
    const v = document.getElementById('player-video');
    if (!v) return;
    try {
        if (document.pictureInPictureElement) {
            await document.exitPictureInPicture();
        } else if (v.requestPictureInPicture) {
            await v.requestPictureInPicture();
        } else {
            notify('❌ PiP не поддерживается браузером', true);
        }
    } catch (e) {
        notify('❌ PiP: ' + e.message, true);
    }
}

async function loadMediaStats() {
    try {
        const r = await fetch('/api/media/stats');
        const d = await r.json();
        document.getElementById('media-stats').textContent = '🎬 ' + (d.movies || 0) + ' · 📺 ' + (d.tv || 0);
    } catch (e) {
        document.getElementById('media-stats').textContent = '';
    }
}

// ==================== ПРОГРЕСС ПРОСМОТРА ====================
let sponsorSegments = [];
let sponsorblockEnabled = localStorage.getItem('sponsorblock-enabled') !== 'false';
let autoNextArmed = false;

async function loadSponsorblock(videoId) {
    if (!videoId) { sponsorSegments = []; return; }
    try {
        const r = await fetch('/api/youtube/sponsorblock?video_id=' + encodeURIComponent(videoId));
        const d = await r.json();
        sponsorSegments = d.segments || [];
    } catch (e) { sponsorSegments = []; }
}
function toggleSponsorblock() {
    sponsorblockEnabled = !sponsorblockEnabled;
    localStorage.setItem('sponsorblock-enabled', sponsorblockEnabled);
    const b = document.getElementById('sb-toggle');
    if (b) b.style.opacity = sponsorblockEnabled ? '1' : '0.4';
    notify(sponsorblockEnabled ? '⏭ SponsorBlock включён' : '⏭ SponsorBlock выключен');
}
function showSponsorSkipToast(category) {
    const m = {'sponsor':'⏭ Пропущен спонсор','selfpromo':'⏭ Пропущена самореклама','interaction':'⏭ Пропущена просьба подписаться','intro':'⏭ Пропущено вступление','outro':'⏭ Пропущена концовка','music_offtopic':'⏭ Пропущено'} ;
    notify(m[category] || '⏭ Пропущен сегмент');
}

function setupPlayer() {
    const v = document.getElementById('player-video');
    v.addEventListener('timeupdate', () => {
        const now = Date.now();
        if (now - lastProgressSave >= 5000) {
            lastProgressSave = now;
            savePlaybackProgress();
        }
        // SponsorBlock: авто-пропуск сегментов
        if (sponsorblockEnabled && sponsorSegments.length) {
            const t = v.currentTime;
            for (const s of sponsorSegments) {
                if (!s.segment || s.segment.length < 2) continue;
                const [start, end] = s.segment;
                if (t >= start && t < end) {
                    v.currentTime = end;
                    showSponsorSkipToast(s.category);
                    break;
                }
            }
        }
        // Авто-следующая: запускаем отсчёт за 10с до конца
        const dur = v.duration || 0;
        if (currentPlayback && currentPlayback.type === 'episode' && dur > 0 && (dur - v.currentTime) <= 10 && (dur - v.currentTime) > 0) {
            if (!autoNextArmed && !autoNextTimer) {
                autoNextArmed = true;
                checkAutoNext(currentPlayback.id);
            }
        }
    });
    v.addEventListener('ended', () => {
        if (currentPlayback) {
            const d = Math.floor(v.duration || v.currentTime || 0);
            savePlaybackProgress(d, d);
            onPlaybackEnded();
        }
    });
    v.addEventListener('loadedmetadata', () => {
        if (resumePosition > 30 && resumePosition < (v.duration || 0) - 30) {
            v.currentTime = resumePosition;
        }
        resumePosition = 0;
    });
}

function savePlaybackProgress(pos, dur) {
    if (!currentPlayback) return;
    const v = document.getElementById('player-video');
    const p = pos !== undefined ? Math.floor(pos) : Math.floor(v.currentTime || 0);
    const d = dur !== undefined ? Math.floor(dur) : Math.floor(v.duration || 0);
    fetch('/api/media/progress', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({media_type: currentPlayback.type, media_id: currentPlayback.id, position_sec: p, duration_sec: d})
    }).catch(() => {});
}

async function beginPlayback(type, id, src) {
    currentPlayback = {type: type, id: id};
    resumePosition = 0;
    lastProgressSave = 0;
    autoNextArmed = false;
    sponsorSegments = [];
    const sb = document.getElementById('sb-toggle');
    if (sb) sb.style.opacity = sponsorblockEnabled ? '1' : '0.4';
    const v = document.getElementById('player-video');
    v.src = src;
    const track = document.getElementById('player-track');
    track.removeAttribute('src');
    track.mode = 'disabled';
    if (type === 'episode') {
        loadSubtitles(id);
    } else {
        document.getElementById('subtitle-select').innerHTML = '<option value="">Выключены</option>';
    }
    document.getElementById('resume-banner').style.display = 'none';
    document.getElementById('player-modal').classList.add('show');
    try {
        const r = await fetch('/api/media/progress/' + type + '/' + id);
        const p = await r.json();
        if (p.position_sec && p.position_sec > 30) {
            resumePosition = p.position_sec;
            document.getElementById('resume-text').textContent = 'Продолжить с ' + formatTime(p.position_sec);
            document.getElementById('resume-banner').style.display = 'flex';
        }
    } catch (e) {}
}

function resumePlayback() {
    const v = document.getElementById('player-video');
    if (resumePosition > 0) v.currentTime = resumePosition;
    resumePosition = 0;
    document.getElementById('resume-banner').style.display = 'none';
    v.play();
}

function restartPlayback() {
    resumePosition = 0;
    document.getElementById('resume-banner').style.display = 'none';
    const v = document.getElementById('player-video');
    v.currentTime = 0;
    v.play();
}

let autoNextEnabled = true;
let autoNextTimer = null;

function onPlaybackEnded() {
    if (currentPlayback && currentPlayback.type === 'episode') {
        checkAutoNext(currentPlayback.id);
    }
}

async function checkAutoNext(id) {
    if (!autoNextEnabled) return;
    try {
        const r = await fetch('/api/media/episode/' + id + '/next');
        const d = await r.json();
        if (d.next && d.next.id) showAutoNextOverlay(d.next);
    } catch (e) {}
}

function showAutoNextOverlay(next) {
    const overlay = document.getElementById('auto-next-overlay');
    document.getElementById('auto-next-title').textContent = 'Следующая: S' + String(next.season).padStart(2, '0') + 'E' + String(next.episode).padStart(2, '0') + (next.title ? ' — ' + next.title : '');
    overlay.style.display = 'flex';
    window._autoNextId = next.id;
    let remaining = 10;
    document.getElementById('auto-next-countdown').textContent = remaining;
    clearInterval(autoNextTimer);
    autoNextTimer = setInterval(() => {
        remaining--;
        if (remaining <= 0) {
            clearInterval(autoNextTimer);
            const nid = window._autoNextId;
            overlay.style.display = 'none';
            window._autoNextId = null;
            if (nid) playEpisode(nid);
        } else {
            document.getElementById('auto-next-countdown').textContent = remaining;
        }
    }, 1000);
}

function cancelAutoNext() {
    clearInterval(autoNextTimer);
    document.getElementById('auto-next-overlay').style.display = 'none';
    window._autoNextId = null;
}

function watchNowAutoNext() {
    clearInterval(autoNextTimer);
    document.getElementById('auto-next-overlay').style.display = 'none';
    const id = window._autoNextId;
    window._autoNextId = null;
    if (id) playEpisode(id);
}

function toggleAutoNext(val) {
    autoNextEnabled = val;
    localStorage.setItem('autoNext', val ? 'true' : 'false');
}

// ==================== СУБТИТРЫ ====================
let currentSubtitleId = null;

async function loadSubtitles(id) {
    const sel = document.getElementById('subtitle-select');
    sel.innerHTML = '<option value="">Выключены</option>';
    try {
        const r = await fetch('/api/media/subtitles/list/' + id);
        const d = await r.json();
        const subs = d.subtitles || [];
        subs.forEach(s => {
            const o = document.createElement('option');
            o.value = s.idx;
            o.textContent = s.label;
            sel.appendChild(o);
        });
        const ru = subs.find(s => s.lang === 'ru');
        if (ru) {
            sel.value = ru.idx;
            setSubtitleTrack(ru.idx);
        }
        currentSubtitleId = id;
    } catch (e) {}
}

function selectSubtitle(idx) {
    setSubtitleTrack(idx);
}

function setSubtitleTrack(idx) {
    const track = document.getElementById('player-track');
    if (!idx || idx === '') {
        track.removeAttribute('src');
        track.mode = 'disabled';
    } else {
        track.src = '/api/media/subtitles/' + currentSubtitleId + '/' + idx;
        track.mode = 'showing';
    }
}

async function loadContinueWatching() {
    const row = document.getElementById('continue-watching');
    try {
        const r = await fetch('/api/media/progress/continue');
        const d = await r.json();
        const items = d.items || [];
        if (items.length === 0) { row.innerHTML = ''; return; }
        let html = '<div style="color:var(--fg-muted);font-size:12px;margin-bottom:6px;">▶️ Продолжить просмотр</div><div class="continue-row">';
        for (const it of items) {
            const info = await resolvePlaybackInfo(it.media_type, it.media_id);
            const pct = Math.min(100, it.percent || 0).toFixed(0);
            const title = info.title || (it.media_type + ' #' + it.media_id);
            const poster = info.poster ? '/api/media/poster?url=' + encodeURIComponent(info.poster) : '';
            html += '<div class="continue-item" onclick="continuePlayback(\'' + it.media_type + '\', ' + it.media_id + ')">';
            html += poster
                ? '<img src="' + poster + '" alt="" loading="lazy" />'
                : '<div style="width:140px;aspect-ratio:2/3;background:var(--bg-button);display:flex;align-items:center;justify-content:center;color:var(--fg-dim);">🎬</div>';
            html += '<div class="continue-title">' + title + '</div>';
            html += '<div class="continue-bar"><div class="continue-bar-fill" style="width:' + pct + '%"></div></div>';
            html += '</div>';
        }
        html += '</div>';
        row.innerHTML = html;
    } catch (e) {
        row.innerHTML = '';
    }
}

async function resolvePlaybackInfo(type, id) {
    try {
        if (type === 'movie') {
            const m = await (await fetch('/api/media/movies/' + id)).json();
            return {title: m.title, poster: m.poster_url};
        } else if (type === 'tv') {
            const m = await (await fetch('/api/media/tv/' + id)).json();
            return {title: m.title, poster: m.poster_url};
        } else if (type === 'episode') {
            const e = await (await fetch('/api/media/episode/' + id)).json();
            return {title: 'S' + String(e.season).padStart(2, '0') + 'E' + String(e.episode).padStart(2, '0') + ' ' + (e.title || ''), poster: e.thumbnail_url};
        }
    } catch (e) {}
    return {title: null, poster: null};
}

function continuePlayback(type, id) {
    if (type === 'episode') {
        mediaType = 'tv';
        playEpisode(id);
    } else {
        mediaType = type;
        openPlayer(id);
    }
}


// ==================== КИНОАФИША ====================
let cinemaCategory = 'now_playing';
let cinemaPage = 1;

function cinemaEscapeJs(s) {
    return String(s || '').replace(/\\/g, '\\\\').replace(/'/g, "\\'");
}

function cinemaPoster(url) {
    return url
        ? '<img src="/api/media/poster?url=' + encodeURIComponent(url) + '" loading="lazy" alt="" onerror="this.outerHTML=\'<div class=&quot;cinema-poster-placeholder&quot;>🎬</div>\'">'
        : '<div class="cinema-poster-placeholder">🎬</div>';
}

async function cinemaTab(category, btn) {
    cinemaCategory = category;
    cinemaPage = 1;
    document.querySelectorAll('.cinema-tab').forEach(t => t.classList.remove('active'));
    if (btn && btn.classList) btn.classList.add('active');
    await loadCinemaContent();
}

async function loadCinemaContent() {
    const container = document.getElementById('cinema-content');
    const pagination = document.getElementById('cinema-pagination');
    if (!container) return;

    if (cinemaCategory === 'wishlist') {
        pagination.innerHTML = '';
        await loadWishlist(container);
        return;
    }

    container.innerHTML = '<div class="cinema-grid">' + skeletonGrid(12) + '</div>';
    pagination.innerHTML = '';
    try {
        const r = await apiFetch('/api/media/tmdb/' + cinemaCategory + '?page=' + cinemaPage);
        const d = await r.json();
        const items = d.items || [];
        if (items.length === 0) {
            container.innerHTML = '<div class="loading">Ничего не найдено</div>';
            return;
        }
        container.innerHTML = renderCinemaGrid(items);
        pagination.innerHTML = renderCinemaPagination(d.page || cinemaPage, d.total_pages || 1);
    } catch (e) {
        container.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

function renderCinemaGrid(items) {
    let html = '<div class="cinema-grid">';
    items.forEach(item => {
        html += '<div class="cinema-card" onclick="openCinemaDetail(' + item.tmdb_id + ',\'' + item.media_type + '\')">';
        html += cinemaPoster(item.poster_url);
        html += '<div class="cinema-title" title="' + escapeHtml(item.title || '') + '">' + escapeHtml(item.title || '') + '</div>';
        html += '<div class="cinema-meta">' + (item.year || '—') + ' · ⭐ ' + (item.rating ? Number(item.rating).toFixed(1) : '—') + '</div>';
        if (item.genres && item.genres.length) {
            html += '<div class="cinema-groups">' + item.genres.slice(0, 3).map(g => '<span>' + escapeHtml(g) + '</span>').join('') + '</div>';
        }
        if (item.in_library) {
            html += '<div class="cinema-badge library">✅ В библиотеке</div>';
        } else {
            html += '<div class="cinema-actions">';
            html += '<button onclick="event.stopPropagation(); cinemaTrailer(' + item.tmdb_id + ',\'' + item.media_type + '\')" title="Трейлер">▶️</button>';
            html += '<button onclick="event.stopPropagation(); cinemaFindTorrent(\'' + cinemaEscapeJs(item.title) + '\',' + (item.year || 0) + ')" title="Найти торрент">🔍</button>';
            html += '<button onclick="event.stopPropagation(); cinemaAddWish(' + item.tmdb_id + ',\'' + item.media_type + '\')" title="Хочу посмотреть">❤️</button>';
            html += '</div>';
        }
        html += '</div>';
    });
    html += '</div>';
    return html;
}

function renderCinemaPagination(page, totalPages) {
    if (totalPages <= 1) return '';
    let html = '<div class="pagination-inline">';
    html += '<button onclick="cinemaPrevPage()" ' + (page <= 1 ? 'disabled' : '') + '>← Назад</button>';
    html += '<span>Стр. ' + page + ' из ' + totalPages + '</span>';
    html += '<button onclick="cinemaNextPage()" ' + (page >= totalPages ? 'disabled' : '') + '>Вперёд →</button>';
    html += '</div>';
    return html;
}

function cinemaPrevPage() { if (cinemaPage > 1) { cinemaPage--; loadCinemaContent(); } }
function cinemaNextPage() { cinemaPage++; loadCinemaContent(); }

async function cinemaAddWish(tmdbId, mediaType) {
    try {
        const r = await apiFetch('/api/media/wishlist', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({tmdb_id: tmdbId, media_type: mediaType})
        });
        const d = await r.json().catch(() => ({}));
        if (r.ok) notify('❤️ Добавлено в список желаемого');
        else notify('❌ ' + (d.error || 'Ошибка'), true);
    } catch (e) { notify('❌ ' + e.message, true); }
}

function cinemaFindTorrent(title, year) {
    showTab('torrents');
    setTimeout(() => {
        const input = document.getElementById('torrent-query');
        if (input) input.value = title + (year ? ' ' + year : '');
        searchTorrentsUI();
    }, 200);
}

async function cinemaTrailer(tmdbId, mediaType) {
    try {
        const r = await apiFetch('/api/media/tmdb/trailer?media_type=' + encodeURIComponent(mediaType) + '&tmdb_id=' + tmdbId);
        const d = await r.json();
        if (d.trailer_url) window.open(d.trailer_url, '_blank');
        else notify('Трейлер не найден', true);
    } catch (e) { notify('❌ ' + e.message, true); }
}

async function loadWishlist(container) {
    container.innerHTML = '<div class="cinema-grid">' + skeletonGrid(8) + '</div>';
    try {
        const r = await apiFetch('/api/media/wishlist');
        const d = await r.json();
        const items = d.items || [];
        if (items.length === 0) {
            container.innerHTML = '<div class="loading">Список пуст. Добавь фильмы через ❤️</div>';
            return;
        }
        let html = '<div class="cinema-grid">';
        items.forEach(item => {
            html += '<div class="cinema-card">';
            html += cinemaPoster(item.poster_url);
            html += '<div class="cinema-title" title="' + escapeHtml(item.title || '') + '">' + escapeHtml(item.title || '') + '</div>';
            html += '<div class="cinema-meta">' + (item.year || '—') + ' · ⭐ ' + (item.rating ? Number(item.rating).toFixed(1) : '—') + (item.release_date ? ' · 📅 ' + item.release_date : '') + '</div>';
            if (item.notified) html += '<div class="cinema-badge library" style="background:var(--accent);">🎬 Вышел</div>';
            html += '<div class="cinema-actions">';
            html += '<button onclick="cinemaFindTorrent(\'' + cinemaEscapeJs(item.title) + '\',' + (item.year || 0) + ')" title="Найти торрент">🔍</button>';
            html += '<button onclick="cinemaRemoveWish(' + item.id + ')" title="Удалить">🗑️</button>';
            html += '</div>';
            html += '</div>';
        });
        html += '</div>';
        container.innerHTML = html;
    } catch (e) {
        container.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

async function cinemaRemoveWish(id) {
    if (!confirm('Удалить из списка?')) return;
    try {
        await apiFetch('/api/media/wishlist/' + id, {method: 'DELETE'});
        notify('🗑️ Удалено');
        loadCinemaContent();
    } catch (e) { notify('❌ ' + e.message, true); }
}

function openCinemaDetail(tmdbId, mediaType) {
    notify('ℹ️ Детали: TMDb #' + tmdbId + ' (' + mediaType + ')');
}

// Бейдж новых релизов из wishlist
let cinemaReleasedCount = 0;

async function refreshCinemaBadge() {
    try {
        const r = await fetch('/api/media/wishlist/released/count');
        const d = await r.json();
        const n = d.count || 0;
        if (n !== cinemaReleasedCount) {
            cinemaReleasedCount = n;
            renderTabsNav();
        }
    } catch (e) {}
}

function resetCinemaBadge() {
    if (cinemaReleasedCount === 0) return;
    cinemaReleasedCount = 0;
    renderTabsNav();
    fetch('/api/media/wishlist/released/reset', {method: 'POST'}).catch(() => {});
}

function renderMusicAlbums(albums) {
    const grid = document.getElementById('music-albums');
    if (!albums.length) { grid.innerHTML = '<div class="loading">Нет альбомов</div>'; return; }
    let html = '';
    albums.forEach(a => {
        const artist = a.artist || '', album = a.album || '';
        html += '<div class="music-album" onclick="showAlbum(\'' + artist.replace(/'/g, "\\'") + '\', \'' + album.replace(/'/g, "\\'") + '\')">';
        html += '<img class="music-album-cover" src="/api/music/covers/' + encodeURIComponent(artist) + '/' + encodeURIComponent(album) + '?v=v6' + '" loading="lazy" onerror="this.style.background=\'var(--bg-button)\'; this.src=\'\'">';
        html += '<div class="music-album-title">' + album + '</div>';
        html += '<div class="music-album-artist">' + artist + '</div>';
        html += '</div>';
    });
    grid.innerHTML = html;
}

let currentAlbum = null;

async function showAlbum(artist, album) {
    currentAlbum = {artist, album};
    document.getElementById('album-title').textContent = album;
    document.getElementById('album-artist').textContent = artist;
    document.getElementById('album-cover').src = '/api/music/covers/' + encodeURIComponent(artist) + '/' + encodeURIComponent(album) + '?v=v6';
    document.getElementById('album-tracks').innerHTML = '<div class="loading">Загрузка...</div>';
    document.getElementById('album-modal').classList.add('show');
    try {
        const r = await fetch('/api/music/albums/' + encodeURIComponent(artist) + '/' + encodeURIComponent(album));
        const d = await r.json();
        const tracks = d.tracks || [];
        let html = '';
        tracks.forEach((t, i) => {
            const isCur = currentPlayingFile === t.file;
            html += '<div class="album-track' + (isCur ? ' playing' : '') + '" onclick="playTrack(\'' + (t.file || '').replace(/'/g, "\\'") + '\')">';
            html += '<span class="track-num">' + (t.track || (i + 1)) + '</span>';
            html += '<span class="track-title">' + escapeHtml(t.title || t.file) + '</span>';
            html += '<span class="track-duration">' + fmtMusicTime(t.duration || 0) + '</span>';
            html += '<span class="track-actions">' + addPlBtn(t.file) + '</span>';
            html += '</div>';
        });
        document.getElementById('album-tracks').innerHTML = html || '<div class="loading">Треков нет</div>';
    } catch (e) {
        document.getElementById('album-tracks').innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

function closeAlbumModal() {
    document.getElementById('album-modal').classList.remove('show');
}

async function playCurrentAlbum() {
    if (!currentAlbum) return;
    try {
        await fetch('/api/music/play/album', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(currentAlbum)});
        musicLoadStatus();
    } catch (e) { notify('❌ Ошибка: ' + e.message, true); }
}

let vizInterval = null;
function startVisualizer() {
    if (vizInterval) return;
    const bars = document.querySelectorAll('.viz-bar');
    vizInterval = setInterval(() => {
        bars.forEach((bar, i) => {
            const vol = musicCurrentVolume / 100;
            const base = Math.sin(Date.now() / 250 + i * 0.5) * 35 + 45;
            const random = Math.random() * 25;
            const height = Math.max(5, Math.min(100, (base + random) * vol));
            bar.style.height = height + '%';
        });
    }, 100);
}
function stopVisualizer() {
    if (vizInterval) { clearInterval(vizInterval); vizInterval = null; }
    document.querySelectorAll('.viz-bar').forEach(bar => bar.style.height = '5%');
}

async function musicSearch() {
    const q = document.getElementById('music-search-input').value.trim();
    if (!q) { musicLoadAlbums(); return; }
    try {
        const r = await fetch('/api/music/search?q=' + encodeURIComponent(q));
        const d = await r.json();
        const results = d.results || [];
        const grid = document.getElementById('music-albums');
        if (!results.length) { grid.innerHTML = '<div class="loading">Ничего не найдено</div>'; return; }
        document.getElementById('music-albums-count').textContent = '(' + results.length + ')';
        let html = '<div class="history-list">';
        results.forEach(s => {
            html += '<div class="history-item" style="cursor:pointer;" onclick="playTrack(\'' + (s.file || '').replace(/'/g, "\\'") + '\')"><span style="flex:1;">' + escapeHtml(s.title || s.file) + '</span><span class="time">' + escapeHtml(s.artist || '') + ' · ' + fmtMusicTime(s.duration) + '</span>' + addPlBtn(s.file) + '</div>';
        });
        html += '</div>';
        grid.innerHTML = html;
    } catch (e) { notify('❌ Ошибка: ' + e.message, true); }
}
