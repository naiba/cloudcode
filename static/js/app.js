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

document.addEventListener('instanceDeleted', function(event) {
    var id = event.detail && event.detail.id;
    if (id) {
        localStorage.removeItem('_cc_store_' + id);
        if (localStorage.getItem('_cc_active_inst') === id) {
            localStorage.removeItem('_cc_active_inst');
        }
    }
    window.location.reload();
});

/* localStorage isolation between opencode instances */
var _CC_KEY = '_cc_active_inst';

function _ccIsShared(n) {
    return n === _CC_KEY || n.startsWith('_cc_store_') ||
        n === 'theme' || n === 'opencode-theme-id' || n === 'opencode-color-scheme' ||
        n.startsWith('opencode-theme-css-');
}

function _ccClearNonShared() {
    var toRemove = [];
    for (var i = localStorage.length; i--;) {
        var n = localStorage.key(i);
        if (!_ccIsShared(n)) toRemove.push(n);
    }
    toRemove.forEach(function(n) { localStorage.removeItem(n); });
}

function _ccRestoreInstance(id) {
    var saved = localStorage.getItem('_cc_store_' + id);
    if (saved) {
        try {
            var d = JSON.parse(saved);
            Object.keys(d).forEach(function(n) { localStorage.setItem(n, d[n]); });
        } catch(e) {}
    }
}

function switchInstance(id) {
    _ccClearNonShared();
    _ccRestoreInstance(id);
    localStorage.setItem(_CC_KEY, id);
    window.open('/instance/' + id + '/', '_blank');
}

document.addEventListener('htmx:beforeSwap', function(event) {
    if (event.detail.xhr.status === 201) {
        var redirect = event.detail.xhr.getResponseHeader('HX-Redirect');
        if (redirect) {
            window.location.href = redirect;
            event.detail.shouldSwap = false;
        }
    }
});

function getToastContainer() {
    var c = document.getElementById('toast-container');
    if (!c) {
        c = document.createElement('div');
        c.id = 'toast-container';
        c.className = 'toast-container';
        document.body.appendChild(c);
    }
    return c;
}

function showToast(message, type) {
    var container = getToastContainer();
    var toast = document.createElement('div');
    toast.className = 'toast toast-' + (type || 'error');
    toast.innerHTML = '<div class="toast-bar"></div><div class="toast-body"></div><div class="toast-progress"></div>';
    toast.querySelector('.toast-body').textContent = message;
    container.appendChild(toast);
    setTimeout(function() {
        toast.classList.add('toast-exit');
        setTimeout(function() { toast.remove(); }, 250);
    }, 5000);
}
