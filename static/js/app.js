// Handle HTMX events
document.addEventListener('htmx:responseError', function(event) {
    const msg = event.detail.xhr.responseText || 'An error occurred';
    showToast(msg, 'error');
});

// Handle instance deleted event - reload the page
document.addEventListener('instanceDeleted', function() {
    window.location.reload();
});

// Handle HX-Redirect
document.addEventListener('htmx:beforeSwap', function(event) {
    if (event.detail.xhr.status === 201) {
        const redirect = event.detail.xhr.getResponseHeader('HX-Redirect');
        if (redirect) {
            window.location.href = redirect;
            event.detail.shouldSwap = false;
        }
    }
});

// Simple toast notification
function showToast(message, type) {
    const toast = document.createElement('div');
    toast.className = 'alert alert-' + (type || 'error');
    toast.style.cssText = 'position:fixed;top:20px;right:20px;z-index:9999;max-width:400px;animation:fadeIn 0.3s';
    toast.textContent = message;
    document.body.appendChild(toast);
    setTimeout(function() {
        toast.style.opacity = '0';
        toast.style.transition = 'opacity 0.3s';
        setTimeout(function() { toast.remove(); }, 300);
    }, 5000);
}
