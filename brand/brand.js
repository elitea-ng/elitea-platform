// Elitea AI brand guide: theme toggle and copy-to-clipboard. No dependencies.
(function () {
  var root = document.documentElement;
  var toggle = document.getElementById('theme-toggle');
  var toast = document.getElementById('toast');
  var order = ['system', 'light', 'dark'];
  var timer = null;

  function current() {
    return root.getAttribute('data-theme') || 'system';
  }
  function label(mode) {
    toggle.textContent = mode.charAt(0).toUpperCase() + mode.slice(1);
    toggle.setAttribute('aria-label', 'Colour theme: ' + mode);
  }
  function setTheme(mode) {
    if (mode === 'system') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', mode);
    try {
      if (mode === 'system') localStorage.removeItem('elitea-brand-theme');
      else localStorage.setItem('elitea-brand-theme', mode);
    } catch (e) {}
    label(mode);
  }
  label(current());
  toggle.addEventListener('click', function () {
    setTheme(order[(order.indexOf(current()) + 1) % order.length]);
  });

  function show(message) {
    toast.textContent = message;
    toast.classList.add('show');
    clearTimeout(timer);
    timer = setTimeout(function () {
      toast.classList.remove('show');
    }, 1800);
  }
  function copy(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(
        function () { show('Copied ' + text); },
        function () { show('Copy failed. Select ' + text + ' by hand.'); }
      );
    } else {
      show('Copy is not available here. Select ' + text + ' by hand.');
    }
  }
  document.addEventListener('click', function (event) {
    var target = event.target.closest('[data-copy]');
    if (target) copy(target.getAttribute('data-copy'));
  });

  // Every fenced code block gets a copy button.
  var blocks = document.querySelectorAll('pre.code');
  for (var i = 0; i < blocks.length; i++) {
    var button = document.createElement('button');
    button.type = 'button';
    button.className = 'copy-code';
    button.textContent = 'Copy';
    button.setAttribute('data-copy-block', String(i));
    blocks[i].appendChild(button);
  }
  document.addEventListener('click', function (event) {
    var target = event.target.closest('[data-copy-block]');
    if (!target) return;
    var code = blocks[Number(target.getAttribute('data-copy-block'))].querySelector('code').textContent;
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(code).then(function () { show('Copied the CSS tokens'); });
    }
  });
})();
