// Applied before CSS to avoid a theme flash; also registers the service worker.
(function () {
    var t = 'mocha';
    try { t = localStorage.getItem('theme') || sessionStorage.getItem('theme') || 'mocha'; } catch (e) {}
    if (t === 'auto') {
        t = (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches) ? 'latte' : 'mocha';
    }
    document.documentElement.setAttribute('data-theme', t);
})();
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js');
