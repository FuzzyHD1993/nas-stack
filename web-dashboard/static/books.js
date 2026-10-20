// NAS Dashboard — books search/download and FB2/EPUB reader.
// Classic script: expects shared globals from app.js.

// ==================== КНИГИ ====================
async function searchBooks() {
    const query = document.getElementById('book-query').value.trim();
    if (!query) return;

    const grid = document.getElementById('books-grid');
    grid.innerHTML = '<div class="loading">🔍 Ищу книги...</div>';

    try {
        const r = await fetch('/api/books/search?q=' + encodeURIComponent(query));
        const d = await r.json();

        if (d.error) {
            grid.innerHTML = '<div class="error">Ошибка: ' + escapeHtml(d.error) + '</div>';
            return;
        }

        allBooks = d.books || [];
        document.getElementById('books-count').textContent = 'Найдено: ' + allBooks.length + ' книг';
        renderBooks();
    } catch (e) {
        grid.innerHTML = '<div class="error">Ошибка: ' + e.message + '</div>';
    }
}

function renderBooks() {
    const grid = document.getElementById('books-grid');
    if (allBooks.length === 0) {
        grid.innerHTML = '<div class="loading">Ничего не найдено</div>';
        return;
    }

    const sortBy = document.getElementById('books-sort').value;
    const filter = document.getElementById('books-filter').value;

    let books = [...allBooks];

    if (filter !== 'all') {
        books = books.filter(b => b.formats && b.formats[filter]);
    }

    books.sort((a, b) => {
        if (sortBy === 'title')  return (a.title || '').localeCompare(b.title || '');
        if (sortBy === 'author') return (a.author || '').localeCompare(b.author || '');
        if (sortBy === 'year')   return (b.year || '').localeCompare(a.year || '');
        return 0;
    });

    let html = '';
    books.forEach(b => {
        const cover = b.cover ? '/api/books/cover?url=' + encodeURIComponent(b.cover) : '';
        const coverHTML = cover
            ? '<img class="book-cover" src="' + cover + '" alt="" loading="lazy" onclick="showModal(\'' + cover + '\')" onerror="this.outerHTML=\'<div class=&quot;book-cover-placeholder&quot;>📚</div>\'" />'
            : '<div class="book-cover-placeholder">📚</div>';

        const formats = b.formats || {};
        const idMatch = String(formats.fb2 || b.id || '').match(/(\d{3,})(?:\D*)$/);
        const bookId = idMatch ? idMatch[1] : '';
        let buttonsHTML = '';
        if (bookId) buttonsHTML += '<button class="fb2" onclick="openBookReader(\'' + bookId + '\')">📖 Читать</button>';
        const bt = escapeHtml(b.title || '');
        if (formats.fb2)  buttonsHTML += '<button class="fb2" data-bk-title="' + bt + '" data-bk-url="' + escapeHtml(formats.fb2) + '" onclick="downloadBook(this.dataset.bkTitle,this.dataset.bkUrl,\'fb2\')">FB2</button>';
        if (formats.epub) buttonsHTML += '<button class="epub" data-bk-title="' + bt + '" data-bk-url="' + escapeHtml(formats.epub) + '" onclick="downloadBook(this.dataset.bkTitle,this.dataset.bkUrl,\'epub\')">EPUB</button>';
        if (formats.mobi) buttonsHTML += '<button class="mobi" data-bk-title="' + bt + '" data-bk-url="' + escapeHtml(formats.mobi) + '" onclick="downloadBook(this.dataset.bkTitle,this.dataset.bkUrl,\'mobi\')">MOBI</button>';
        if (!buttonsHTML) buttonsHTML = '<button disabled>Нет форматов</button>';

        html += '<div class="book-card">';
        html += coverHTML;
        html += '<div class="book-info">';
        html += '<div class="book-title" title="' + bt + '">' + bt + '</div>';
        html += '<div class="book-author">' + escapeHtml(b.author || 'Неизвестен') + '</div>';
        html += '<div class="book-meta">';
        html += '<span>' + escapeHtml(b.year || '—') + '</span>';
        html += '<span>' + escapeHtml(b.language || '—') + '</span>';
        html += '</div>';
        html += '<div class="book-formats">' + buttonsHTML + '</div>';
        html += '</div>';
        html += '</div>';
    });

    grid.innerHTML = html;
}

async function downloadBook(title, url, format) {
    notify('⬇️ Скачиваю: ' + title);
    try {
        const r = await fetch('/api/books/download', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({id: url, title: title, format: format})
        });
        const d = await r.json();
        if (d.error) {
            notify('❌ Ошибка: ' + d.error, true);
        } else {
            notify('✅ Скачано: ' + title);
            loadDownloaded();
        }
    } catch (e) {
        notify('❌ Ошибка: ' + e.message, true);
    }
}

async function loadDownloaded() {
    const dl = document.getElementById('downloaded');
    if (dl) dl.innerHTML = skeletonGrid(8);
    try {
        const r = await fetch('/api/books/downloaded');
        const d = await r.json();
        const count = d.count || 0;
        document.getElementById('downloaded-count').textContent = '(' + count + ')';

        if (count === 0) {
            document.getElementById('downloaded').innerHTML = '<div style="color:var(--fg-dim); text-align:center;">Пока ничего не скачано</div>';
            return;
        }

        let html = '<div class="history-list">';
        d.books.sort((a, b) => new Date(b.modtime) - new Date(a.modtime));
        d.books.forEach(b => {
            const date = new Date(b.modtime).toLocaleString('ru-RU');
            const safeName = escapeHtml(b.name);
            const isBook = /\.(epub|fb2)$/i.test(b.name);
            html += '<div class="history-item">';
            html += '<span>📚 ' + safeName + '</span>';
            html += '<span class="time">' + formatSize(b.size) + ' · ' + escapeHtml(date) + '</span>';
            if (isBook) {
                html += '<button data-name="' + safeName + '" onclick="openReader(this.dataset.name)" style="padding:4px 10px;font-size:11px;margin-left:10px;background:var(--accent);color:var(--bg);">📖 Читать</button>';
            }
            html += '<button data-name="' + safeName + '" onclick="openLocal(this.dataset.name)" style="padding:4px 10px;font-size:11px;margin-left:6px;background:var(--success);color:var(--bg);">📂 Открыть</button>';
            html += '</div>';
        });
        html += '</div>';
        document.getElementById('downloaded').innerHTML = html;
    } catch (e) {
        document.getElementById('downloaded').innerHTML = 'Ошибка: ' + e.message;
    }
}

async function openLocal(name) {
    try {
        const r = await fetch(NAS_CONFIG.booksOpenerUrl + '/open?name=' + encodeURIComponent(name), {
            method: 'POST',
            headers: {'X-Opener': '1'}
        });
        const d = await r.json();
        if (d.error) {
            notify('❌ Ошибка: ' + d.error, true);
        } else {
            notify('📂 Открыто: ' + name);
        }
    } catch (e) {
        notify('❌ Демон не запущен: ' + e.message, true);
    }
}

async function openFolder() {
    try {
        const r = await fetch(NAS_CONFIG.booksOpenerUrl + '/open-folder', {
            method: 'POST',
            headers: {'X-Opener': '1'}
        });
        const d = await r.json();
        if (d.error) {
            notify('❌ Ошибка: ' + d.error, true);
        } else {
            notify('📁 Папка открыта');
        }
    } catch (e) {
        notify('❌ Демон не запущен: ' + e.message, true);
    }
}

function showModal(src) {
    document.getElementById('modal-img').src = src;
    document.getElementById('modal').classList.add('show');
}


// ==================== FOLIATE-READER (FB2/EPUB) ====================
let foliateView = null;
let foliateActive = false;

async function openBookReader(id, title) {
    return openFoliateReader('/api/books/read/' + encodeURIComponent(id), title || ('Книга #' + id), 'fb2');
}

async function openFoliateReader(url, title, formatHint) {
    const modal = document.getElementById('reader-modal');
    const epubView = document.getElementById('reader-view');
    const container = document.getElementById('foliate-view');
    if (!modal || !container) return;

    try { if (readerBook) readerBook.destroy(); } catch (e) {}
    readerBook = null;
    readerRendition = null;
    if (foliateView) {
        try { foliateView.close?.(); } catch (e) {}
        try { foliateView.remove(); } catch (e) {}
        foliateView = null;
    }
    container.innerHTML = '';
    container.style.display = 'block';
    if (epubView) epubView.style.display = 'none';
    modal.classList.add('show');
    foliateActive = true;
    document.getElementById('reader-title').textContent = title || 'Чтение';
    document.getElementById('reader-page').textContent = '…';

    try {
        const res = await fetch(url);
        if (!res.ok) {
            let msg = 'HTTP ' + res.status;
            try { const j = await res.json(); if (j && j.error) msg = j.error; } catch (e) {}
            throw new Error(msg);
        }
        const blob = await res.blob();
        const ct = (res.headers.get('content-type') || '').toLowerCase();
        const isFb2 = formatHint === 'fb2' || ct.includes('fb2') || ct.includes('fictionbook')
            || /\.fb2($|\?)/i.test(url);
        // view.js регистрирует custom element <foliate-view> и экспортирует makeBook.
        // Без этого импорта createElement('foliate-view') возвращает пустой HTMLElement,
        // у которого нет .open() — даже если книга открывается через makeFB2.
        const { makeBook } = await import('/static/foliate-js/view.js');
        await customElements.whenDefined('foliate-view');
        // view.open() импортирует paginator.js динамически, но element может
        // создаться до регистрации. Импортируем и ждём регистрацию заранее.
        await import('/static/foliate-js/paginator.js');
        await customElements.whenDefined('foliate-paginator');
        let book;
        if (isFb2) {
            const { makeFB2 } = await import('/static/foliate-js/fb2.js');
            book = await makeFB2(blob);
        } else {
            const isEpub = formatHint === 'epub' || ct.includes('epub') || /\.epub($|\?)/i.test(url);
            const fileName = isEpub ? 'book.epub' : 'book.bin';
            const fileType = isEpub ? 'application/epub+zip' : (ct || 'application/octet-stream');
            book = await makeBook(new File([blob], fileName, { type: fileType }));
        }
        foliateView = document.createElement('foliate-view');
        if (typeof foliateView.open !== 'function') {
            throw new Error('foliate-view не зарегистрирован (view.js не загрузился)');
        }
        foliateView.style.cssText = 'display:block;width:100%;height:100%;';
        container.appendChild(foliateView);
        foliateView.addEventListener('relocate', (e) => {
            const frac = e.detail && e.detail.fraction;
            if (typeof frac === 'number') {
                document.getElementById('reader-page').textContent = Math.round(frac * 100) + '%';
            }
        });
        await foliateView.open(book);
        try {
            await foliateView.init({ showTextStart: true });
        } catch (initErr) {
            console.warn('foliate init fallback:', initErr);
            foliateView.renderer?.next();
        }
        const metaTitle = book.metadata && book.metadata.title;
        if (metaTitle) document.getElementById('reader-title').textContent = metaTitle;
        applyFoliateTheme();
    } catch (e) {
        console.error('Reader error:', e);
        container.innerHTML = '<div style="padding:40px;text-align:center;color:var(--error);">' +
            '<h3>❌ Не удалось открыть книгу</h3>' +
            '<p>' + escapeHtml(e.message) + '</p>' +
            '<p><a href="' + url + '" style="color:var(--accent);">Скачать файл</a></p></div>';
    }
}

function applyFoliateTheme() {
    if (!foliateView || !foliateView.renderer) return;
    const theme = storeGet('reader_theme') || 'dark';
    const fontPct = parseInt(storeGet('reader_font') || '100', 10);
    const css = readerThemeCSS(theme) + '\nhtml { font-size: ' + fontPct + '% !important; }';
    try { foliateView.renderer.setStyles?.(css); } catch (e) {}
}

// ==================== EPUB-ЧИТАЛКА ====================
let readerBook = null;
let readerRendition = null;
let readerCurrentFile = null;

function openReader(filename) {
    if (!filename) return;
    if (/\.fb2$/i.test(filename)) {
        openFoliateReader('/api/books/read/' + encodeURIComponent(filename), filename, 'fb2');
        return;
    }
    if (typeof ePub === 'undefined') {
        notify('❌ epub.js не загружен', true);
        return;
    }
    closeReader();
    readerCurrentFile = filename;
    const modal = document.getElementById('reader-modal');
    document.getElementById('reader-title').textContent = filename;
    document.getElementById('reader-page').textContent = '—';
    modal.classList.add('show');

    const url = '/api/books/read/' + encodeURIComponent(filename);
    readerBook = ePub(url);
    readerRendition = readerBook.renderTo('reader-view', {
        width: '100%',
        height: '100%',
        spread: 'none',
        flow: 'paginated'
    });

    const fontPct = parseInt(storeGet('reader_font') || '100', 10);
    readerRendition.themes.fontSize(fontPct + '%');

    // Тема применяется инверсией прямо к iframe книги (книжные стили перебивают epub themes)
    readerRendition.on('rendered', () => { applyReaderTheme(); });

    readerRendition.display().then(() => {
        applyReaderTheme();
        const saved = storeGet('reader_' + filename);
        if (saved) {
            readerRendition.display(saved).then(() => {
                applyReaderTheme();
            }).catch(() => {});
        }
    }).catch((e) => notify('❌ Читалка: ' + e.message, true));

    readerBook.ready.then(() => {
        try {
            const md = readerBook.packaging.metadata;
            if (md && md.title) document.getElementById('reader-title').textContent = md.title;
        } catch (e) {}
    }).catch(() => {});

    readerRendition.on('relocated', (location) => {
        if (!location || !location.start) return;
        const cfi = location.start.cfi;
        if (cfi) storeSet('reader_' + filename, cfi);
        const d = location.start.displayed;
        if (d) document.getElementById('reader-page').textContent = d.page + ' / ' + d.total;
    });

    document.addEventListener('keydown', readerKeyHandler);
}

function readerKeyHandler(e) {
    const modal = document.getElementById('reader-modal');
    if (!modal || !modal.classList.contains('show')) return;
    if (e.key === 'ArrowLeft') { e.preventDefault(); readerPrev(); }
    else if (e.key === 'ArrowRight') { e.preventDefault(); readerNext(); }
    else if (e.key === 'Escape') { e.preventDefault(); closeReader(); }
}

function readerPrev() { if (foliateActive && foliateView) foliateView.prev(); else if (readerRendition) readerRendition.prev(); }
function readerNext() { if (foliateActive && foliateView) foliateView.next(); else if (readerRendition) readerRendition.next(); }

function readerChangeFont(delta) {
    const current = parseInt(storeGet('reader_font') || '100', 10);
    const next = Math.max(50, Math.min(200, current + delta * 10));
    storeSet('reader_font', String(next));
    if (readerRendition) readerRendition.themes.fontSize(next + '%');
    applyFoliateTheme();
    notify('🔤 Шрифт: ' + next + '%');
}

function toggleReaderTheme() {
    const current = storeGet('reader_theme') || 'dark';
    const next = current === 'dark' ? 'light' : 'dark';
    storeSet('reader_theme', next);
    applyReaderTheme();
    applyFoliateTheme();
}

// Тема применяется прямой инъекцией CSS в iframe книги (стили книги перебивают epub.js themes).
// Фон и цвет текста задаются с !important, картинки не трогаем — они остаются оригинальными.
function readerThemeCSS(theme) {
    if (theme === 'light') {
        return 'html, body { background-color: #ffffff !important; }\n' +
               'body, body * { color: #1a1a1a !important; }\n' +
               'a, a * { color: #0066cc !important; }\n' +
               'img, svg, video, image { background: transparent !important; }';
    }
    return 'html, body { background-color: #1e1e2e !important; }\n' +
           'body, body * { color: #cdd6f4 !important; }\n' +
           'a, a * { color: #89b4fa !important; }\n' +
           'img, svg, video, image { background: transparent !important; }';
}

function applyReaderTheme() {
    if (!readerRendition) return;
    const theme = storeGet('reader_theme') || 'dark';
    const btn = document.getElementById('reader-theme-btn');

    try {
        const contents = readerRendition.getContents() || [];
        contents.forEach(content => {
            const doc = content.document || (content.content && content.content.document);
            if (!doc || !doc.documentElement) return;
            let styleEl = doc.getElementById('nas-reader-theme');
            if (!styleEl) {
                styleEl = doc.createElement('style');
                styleEl.id = 'nas-reader-theme';
                (doc.head || doc.documentElement).appendChild(styleEl);
            }
            styleEl.textContent = readerThemeCSS(theme);
        });
    } catch (e) {
        console.error('Reader theme failed:', e);
    }

    if (btn) btn.textContent = theme === 'light' ? '☀️' : '🌙';
}

function closeReader() {
    try { if (readerBook) readerBook.destroy(); } catch (e) {}
    readerBook = null;
    readerRendition = null;
    try {
        if (foliateView) {
            foliateView.close?.();
            foliateView.remove();
        }
    } catch (e) {}
    foliateView = null;
    foliateActive = false;
    const fv = document.getElementById('foliate-view');
    if (fv) { fv.innerHTML = ''; fv.style.display = 'none'; }
    const rv = document.getElementById('reader-view');
    if (rv) rv.style.display = '';
    const modal = document.getElementById('reader-modal');
    if (modal) modal.classList.remove('show');
    document.removeEventListener('keydown', readerKeyHandler);
}
