// NAS Dashboard — final initialization (runs after all feature scripts).

// ==================== INIT ====================
renderTabsNav();
showTab('today');
updateCastVisibility();
loadProfiles();
checkUpdates();
setInterval(checkUpdates, 6 * 60 * 60 * 1000);
checkHealthDot();
setPolling(checkHealthDot, 30000);
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
setPolling(pollNotifications, 30000);
setPolling(() => { if (currentTab === 'music') musicLoadStatus(); }, 10000);
globalMusicPoll();
setPolling(globalMusicPoll, 15000);
loadExtendedStats();
setPolling(loadExtendedStats, 15000);
setPolling(loadStats, 15000);
setPolling(loadStatus, 30000);
setPolling(loadTorrents, 10000);
setPolling(loadYoutubeJobs, 10000);
setPolling(loadHealth, 60000);
setupBitPerfectToggle();
loadBackgroundPhoto();
registerPush();
refreshNewMediaBadge();
setPolling(refreshNewMediaBadge, 30000);
refreshCinemaBadge();
setPolling(refreshCinemaBadge, 30000);

if (storeGet('searchResultsCollapsed') === 'true') {
    const content = document.getElementById('search-results-content');
    const btn = document.getElementById('toggle-search-btn');
    if (content) content.style.display = 'none';
    if (btn) btn.textContent = '▸ Развернуть';
}
