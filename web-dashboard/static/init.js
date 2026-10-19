// NAS Dashboard — final initialization (runs after all feature scripts).

// ==================== INIT ====================
renderTabsNav();
showTab('today');
updateCastVisibility();
loadProfiles();
checkUpdates();
setInterval(checkUpdates, 6 * 60 * 60 * 1000);
checkHealthDot();
setInterval(checkHealthDot, 30000);
if (!authToken && NAS_AUTH_ENABLED) showLogin();
checkAuth();
window.addEventListener('scroll', () => {
    const b = document.getElementById('scroll-top');
    if (b) b.classList.toggle('show', window.scrollY > 400);
});
loadStats();
loadStatus();
loadTorrents();
loadIndexers();
loadDestinations().then(fillDestinationsSelect);
loadYoutubeJobs();
fillThemeSelect();
setupPlayer();
autoNextEnabled = storeGet('autoNext') !== 'false';
const ant = document.getElementById('auto-next-toggle');
if (ant) ant.checked = autoNextEnabled;
requestNotifPermission();
pollNotifications();
setInterval(pollNotifications, 10000);
setInterval(() => { if (currentTab === 'music') musicLoadStatus(); }, 3000);
globalMusicPoll();
setInterval(globalMusicPoll, 5000);
loadExtendedStats();
setInterval(loadExtendedStats, 5000);
setInterval(loadStats, 5000);
setInterval(loadStatus, 10000);
setInterval(loadTorrents, 3000);
setInterval(loadYoutubeJobs, 2000);
setInterval(loadHealth, 30000);
setupBitPerfectToggle();
loadBackgroundPhoto();
registerPush();
refreshNewMediaBadge();
setInterval(refreshNewMediaBadge, 15000);
refreshCinemaBadge();
setInterval(refreshCinemaBadge, 30000);

if (storeGet('searchResultsCollapsed') === 'true') {
    const content = document.getElementById('search-results-content');
    const btn = document.getElementById('toggle-search-btn');
    if (content) content.style.display = 'none';
    if (btn) btn.textContent = '▸ Развернуть';
}
