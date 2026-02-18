(function() {
    var THEME_KEY = 'theme';
    var CYCLE = ['auto', 'dark', 'light'];

    function getStored() {
        return localStorage.getItem(THEME_KEY) || 'auto';
    }

    function resolveTheme(pref) {
        if (pref === 'auto') {
            return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
        }
        return pref;
    }

    function applyTheme(pref) {
        var resolved = resolveTheme(pref);
        if (resolved === 'light') {
            document.documentElement.setAttribute('data-theme', 'light');
        } else {
            document.documentElement.removeAttribute('data-theme');
        }
        updateIcon(pref);
    }

    function updateIcon(pref) {
        var iconSun = document.getElementById('icon-sun');
        var iconMoon = document.getElementById('icon-moon');
        var iconAuto = document.getElementById('icon-auto');
        if (!iconSun) return;
        iconSun.style.display = 'none';
        iconMoon.style.display = 'none';
        iconAuto.style.display = 'none';
        if (pref === 'light') {
            iconSun.style.display = 'block';
        } else if (pref === 'dark') {
            iconMoon.style.display = 'block';
        } else {
            iconAuto.style.display = 'block';
        }
    }

    function cycleTheme() {
        var current = getStored();
        var idx = CYCLE.indexOf(current);
        var next = CYCLE[(idx + 1) % CYCLE.length];
        localStorage.setItem(THEME_KEY, next);
        applyTheme(next);
    }

    applyTheme(getStored());

    window.matchMedia('(prefers-color-scheme: light)').addEventListener('change', function() {
        if (getStored() === 'auto') {
            applyTheme('auto');
        }
    });

    document.addEventListener('DOMContentLoaded', function() {
        updateIcon(getStored());
        var btn = document.getElementById('theme-toggle');
        if (btn) {
            btn.addEventListener('click', cycleTheme);
        }
    });
})();

document.addEventListener('htmx:responseError', function(event) {
    var msg = event.detail.xhr.responseText || 'An error occurred';
    showToast(msg, 'error');
});

document.addEventListener('instanceDeleted', function() {
    window.location.reload();
});

document.addEventListener('htmx:beforeSwap', function(event) {
    if (event.detail.xhr.status === 201) {
        var redirect = event.detail.xhr.getResponseHeader('HX-Redirect');
        if (redirect) {
            window.location.href = redirect;
            event.detail.shouldSwap = false;
        }
    }
});

function showToast(message, type) {
    var toast = document.createElement('div');
    toast.className = 'alert alert-' + (type || 'error');
    toast.style.cssText = 'position:fixed;top:20px;right:20px;z-index:9999;max-width:400px';
    toast.textContent = message;
    document.body.appendChild(toast);
    setTimeout(function() {
        toast.style.opacity = '0';
        toast.style.transition = 'opacity 0.3s';
        setTimeout(function() { toast.remove(); }, 300);
    }, 5000);
}
