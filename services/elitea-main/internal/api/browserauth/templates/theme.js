(function () {
  // The web app's colour mode (shared/brand/constants.ts `modeStorageKey`):
  // "light", "dark" or "system". Anything else, or nothing, follows the OS,
  // which the stylesheet does on its own when no data-el-scheme is set.
  var key = "el-mode";
  var modes = ["system", "light", "dark"];
  var names = { system: "System", light: "Light", dark: "Dark" };
  var root = document.documentElement;

  function stored() {
    try {
      var value = window.localStorage.getItem(key);
      if (value === "light" || value === "dark" || value === "system") {
        return value;
      }
    } catch (error) {
      // Storage unavailable: follow the OS.
    }
    return "system";
  }

  function apply(mode) {
    if (mode === "light" || mode === "dark") {
      root.setAttribute("data-el-scheme", mode);
    } else {
      root.removeAttribute("data-el-scheme");
    }
    root.setAttribute("data-el-mode", mode);
    var toggle = document.getElementById("theme-toggle");
    if (toggle) {
      var next = modes[(modes.indexOf(mode) + 1) % modes.length];
      var label = "Theme: " + names[mode] + ". Switch to " + names[next];
      toggle.setAttribute("aria-label", label);
      toggle.setAttribute("title", label);
    }
  }

  apply(stored());

  window.addEventListener("storage", function (event) {
    if (event.key === key || event.key === null) {
      apply(stored());
    }
  });

  document.addEventListener("DOMContentLoaded", function () {
    var toggle = document.getElementById("theme-toggle");
    if (!toggle) {
      return;
    }
    toggle.addEventListener("click", function () {
      var current = root.getAttribute("data-el-mode") || "system";
      var next = modes[(modes.indexOf(current) + 1) % modes.length];
      try {
        window.localStorage.setItem(key, next);
      } catch (error) {
        // Not persisted; the choice still applies to this page.
      }
      apply(next);
    });
    apply(root.getAttribute("data-el-mode") || "system");
    toggle.hidden = false;
  });
})();
