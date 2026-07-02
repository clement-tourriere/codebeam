// Repo bulk selection: a "Select all" that checks the visible rows, an escalation
// to "select all N matching" (acts on the whole filtered set, even rows beyond the
// display cap), and Activate/Deactivate that enable only when something is selected.
(function () {
  function setMatchAll(bulk, on) {
    var input = bulk.querySelector("[data-match-all]");
    if (input) {
      input.value = on ? "1" : "";
    }
  }

  function checkVisible(bulk, on) {
    bulk.querySelectorAll("[data-repo-checkbox]").forEach(function (checkbox) {
      checkbox.checked = on;
    });
  }

  function refreshBulkState() {
    document.querySelectorAll("[data-repo-bulk]").forEach(function (bulk) {
      var checkboxes = Array.from(bulk.querySelectorAll("[data-repo-checkbox]"));
      var checked = checkboxes.filter(function (c) {
        return c.checked;
      });
      var matchInput = bulk.querySelector("[data-match-all]");
      var matchAll = !!(matchInput && matchInput.value === "1");
      var total = parseInt(bulk.getAttribute("data-total") || "0", 10);
      var shown = parseInt(bulk.getAttribute("data-shown") || String(checkboxes.length), 10);
      var allVisibleChecked = checkboxes.length > 0 && checked.length === checkboxes.length;
      var anySelected = matchAll || checked.length > 0;

      var selectAll = bulk.querySelector("[data-repo-select-all]");
      if (selectAll) {
        selectAll.checked = matchAll || allVisibleChecked;
        selectAll.indeterminate = !matchAll && checked.length > 0 && !allVisibleChecked;
      }
      var count = bulk.querySelector("[data-repo-selected-count]");
      if (count) {
        count.textContent = String(matchAll ? total : checked.length);
      }
      bulk.querySelectorAll("[data-bulk-action]").forEach(function (btn) {
        btn.disabled = !anySelected;
      });
      var clear = bulk.querySelector("[data-clear-selection]");
      if (clear) {
        clear.hidden = !anySelected;
      }

      var banner = bulk.querySelector("[data-selectall-banner]");
      if (banner) {
        var text = banner.querySelector("[data-selectall-text]");
        var moreLink = banner.querySelector("[data-select-all-matching]");
        if (matchAll) {
          banner.classList.remove("hidden");
          if (text) {
            text.textContent = "All " + total + " matching repositories are selected.";
          }
          if (moreLink) {
            moreLink.hidden = true;
          }
        } else if (total > shown && allVisibleChecked) {
          banner.classList.remove("hidden");
          if (text) {
            text.textContent = "All " + shown + " shown are selected.";
          }
          if (moreLink) {
            moreLink.hidden = false;
          }
        } else {
          banner.classList.add("hidden");
        }
      }
    });
  }

  document.addEventListener("change", function (event) {
    var target = event.target;
    if (!(target instanceof HTMLInputElement)) {
      return;
    }
    var bulk = target.closest("[data-repo-bulk]");
    if (!bulk) {
      return;
    }
    if (target.matches("[data-repo-select-all]")) {
      checkVisible(bulk, target.checked);
      if (!target.checked) {
        setMatchAll(bulk, false);
      }
      refreshBulkState();
      return;
    }
    if (target.matches("[data-repo-checkbox]")) {
      // A manual row change drops the "all matching" escalation.
      setMatchAll(bulk, false);
      refreshBulkState();
    }
  });

  document.addEventListener("click", function (event) {
    if (!(event.target instanceof Element)) {
      return;
    }
    var bulk = event.target.closest("[data-repo-bulk]");
    if (!bulk) {
      return;
    }
    if (event.target.closest("[data-select-all-matching]")) {
      event.preventDefault();
      setMatchAll(bulk, true);
      checkVisible(bulk, true);
      refreshBulkState();
      return;
    }
    if (event.target.closest("[data-clear-selection]")) {
      event.preventDefault();
      setMatchAll(bulk, false);
      checkVisible(bulk, false);
      var selectAll = bulk.querySelector("[data-repo-select-all]");
      if (selectAll) {
        selectAll.checked = false;
        selectAll.indeterminate = false;
      }
      refreshBulkState();
    }
  });

  document.addEventListener("DOMContentLoaded", refreshBulkState);
  document.addEventListener("htmx:afterSwap", refreshBulkState);

  // The repo list re-renders itself every 2s while index jobs run (and when the
  // filter box refetches it), which used to wipe any in-progress selection.
  // Capture the selection before a GET-driven swap of #repo-list and restore it
  // afterwards. POST swaps (bulk actions, toggles) intentionally reset it.
  var savedSelection = null;

  document.addEventListener("htmx:beforeSwap", function (event) {
    var target = event.detail && event.detail.target;
    if (!target || target.id !== "repo-list") {
      return;
    }
    var verb = event.detail.requestConfig && event.detail.requestConfig.verb;
    if (verb !== "get") {
      savedSelection = null;
      return;
    }
    var matchInput = target.querySelector("[data-match-all]");
    savedSelection = {
      ids: Array.from(target.querySelectorAll("[data-repo-checkbox]:checked")).map(function (c) {
        return c.value;
      }),
      matchAll: !!(matchInput && matchInput.value === "1"),
    };
  });

  document.addEventListener("htmx:afterSwap", function () {
    // Only a #repo-list swap can follow its own beforeSwap capture, so a saved
    // selection always belongs to the freshly swapped-in list.
    if (!savedSelection) {
      return;
    }
    var saved = savedSelection;
    savedSelection = null;
    var bulk = document.getElementById("repo-list");
    if (!bulk) {
      return;
    }
    saved.ids.forEach(function (id) {
      var checkbox = bulk.querySelector('[data-repo-checkbox][value="' + id + '"]');
      if (checkbox) {
        checkbox.checked = true;
      }
    });
    if (saved.matchAll) {
      setMatchAll(bulk, true);
      checkVisible(bulk, true);
    }
    refreshBulkState();
  });
})();

// Theme toggle (light <-> dark), persisted in localStorage. The initial theme
// is applied by an inline script in <head> to avoid a flash.
(function () {
  function applyTheme(theme) {
    document.documentElement.dataset.theme = theme;
    try {
      localStorage.setItem("codebeam-theme", theme);
    } catch (e) {}
  }

  document.addEventListener("click", function (event) {
    var toggle = event.target.closest ? event.target.closest("#theme-toggle") : null;
    if (!toggle) {
      return;
    }
    var current = document.documentElement.dataset.theme || "codebeam";
    applyTheme(current === "codebeam-dark" ? "codebeam" : "codebeam-dark");
  });
})();

// File-tree active state: highlight the clicked file immediately (the server
// also sets it on a full page load).
(function () {
  document.addEventListener("click", function (event) {
    var link = event.target.closest ? event.target.closest("[data-tree-file]") : null;
    if (!link) {
      return;
    }
    var tree = link.closest("[data-tree]");
    if (tree) {
      tree.querySelectorAll("[data-tree-file].menu-active").forEach(function (el) {
        el.classList.remove("menu-active");
      });
    }
    link.classList.add("menu-active");
  });
})();

// Code viewer: scroll the focused line into view. A deep link from a search
// result uses ?line=N (so the server can highlight the line), which gives no
// #fragment for the browser to scroll to, and the code scrolls inside #repo-main
// rather than the window — so center the highlighted line ourselves.
(function () {
  function scrollToFocusedLine() {
    var focused = document.querySelector(".code-line-focus");
    if (!focused) {
      return;
    }
    requestAnimationFrame(function () {
      focused.scrollIntoView({ block: "center", inline: "nearest" });
    });
  }

  document.addEventListener("DOMContentLoaded", scrollToFocusedLine);
  document.addEventListener("htmx:afterSettle", scrollToFocusedLine);
})();

// Repo page: keep the always-visible "Search this repo" header field in sync
// with the URL. Searching from the overview box only swaps the results pane (not
// the header), so without this the header input stays empty until a full reload.
// We skip the sync while the field is focused so it never clobbers active typing.
(function () {
  function syncRepoSearch() {
    var input = document.querySelector("[data-repo-search-input]");
    if (!input || input === document.activeElement) {
      return;
    }
    var params = new URLSearchParams(window.location.search);
    input.value = params.get("q") || "";
    var norm = document.querySelector("[data-repo-norm]");
    if (norm) {
      norm.checked = params.get("norm") === "1";
    }
  }

  document.addEventListener("DOMContentLoaded", syncRepoSearch);
  document.addEventListener("htmx:afterSettle", function (e) {
    // Only sync after a main-pane navigation (search results, file, overview),
    // not a tree-folder expansion, so an unsubmitted query is never clobbered.
    if (e && e.detail && e.detail.target && e.detail.target.id === "repo-main") {
      syncRepoSearch();
    }
  });
})();

// Search page: searchable multi-repository picker for large indexes.
(function () {
  var MAX_VISIBLE_OPTIONS = 80;

  function normalizeSearchText(value) {
    var text = String(value || "").toLowerCase();
    if (text.normalize) {
      text = text.normalize("NFD").replace(/[\u0300-\u036f]/g, "");
    }
    return text;
  }

  function uniqueValues(values) {
    var out = [];
    values.forEach(function (value) {
      value = String(value || "").trim();
      if (value !== "" && out.indexOf(value) === -1) {
        out.push(value);
      }
    });
    return out;
  }

  function initRepoCombobox(root) {
    if (!root || root.dataset.repoComboboxReady === "true") {
      return;
    }
    root.dataset.repoComboboxReady = "true";

    var input = root.querySelector("[data-repo-combobox-input]");
    var hiddenContainer = root.querySelector("[data-repo-combobox-hidden]");
    var tokenList = root.querySelector("[data-repo-combobox-selected]");
    var menu = root.querySelector("[data-repo-combobox-menu]");
    var options = Array.from(root.querySelectorAll("[data-repo-option]"));
    var empty = root.querySelector("[data-repo-combobox-empty]");
    var status = root.querySelector("[data-repo-combobox-status]");
    var clear = root.querySelector("[data-repo-combobox-clear]");
    var activeOption = null;
    var selected = [];

    if (!input || !hiddenContainer || !tokenList || !menu || options.length === 0) {
      return;
    }

    options.forEach(function (option, index) {
      if (!option.id) {
        option.id = "repo-combobox-option-" + Date.now() + "-" + index;
      }
      option.dataset.normalizedSearch = normalizeSearchText(
        (option.getAttribute("data-search") || "") + " " +
          (option.getAttribute("data-label") || "") + " " +
          (option.getAttribute("data-value") || "")
      );
    });

    function optionValue(option) {
      return option ? option.getAttribute("data-value") || "" : "";
    }

    function optionLabel(option) {
      return option ? option.getAttribute("data-label") || optionValue(option) : "";
    }

    function findByValue(value) {
      return options.find(function (option) {
        return optionValue(option) === value;
      });
    }

    function findExact(text) {
      var normalized = normalizeSearchText(text.trim());
      return options.find(function (option) {
        return optionValue(option) !== "" && (normalizeSearchText(optionLabel(option)) === normalized || normalizeSearchText(optionValue(option)) === normalized);
      });
    }

    function selectedSet() {
      var set = {};
      selected.forEach(function (value) {
        set[value] = true;
      });
      return set;
    }

    function updateClear() {
      if (clear) {
        clear.classList.toggle("invisible", selected.length === 0 && input.value.trim() === "");
      }
    }

    function visibleOptions() {
      return options.filter(function (option) {
        return !option.hidden;
      });
    }

    function setActive(option) {
      if (activeOption) {
        activeOption.removeAttribute("aria-selected");
        activeOption.classList.remove("bg-base-200");
      }
      activeOption = option || null;
      if (activeOption) {
        activeOption.setAttribute("aria-selected", "true");
        activeOption.classList.add("bg-base-200");
        input.setAttribute("aria-activedescendant", activeOption.id);
        activeOption.scrollIntoView({ block: "nearest" });
      } else {
        input.removeAttribute("aria-activedescendant");
      }
    }

    function renderSelected() {
      hiddenContainer.innerHTML = "";
      tokenList.innerHTML = "";

      selected.forEach(function (value) {
        var option = findByValue(value);
        var label = optionLabel(option) || value;

        var hidden = document.createElement("input");
        hidden.type = "hidden";
        hidden.name = "repo";
        hidden.value = value;
        hiddenContainer.appendChild(hidden);

        var token = document.createElement("span");
        token.className = "badge badge-primary badge-lg max-w-full gap-1";
        token.title = value;

        var text = document.createElement("span");
        text.className = "truncate";
        text.textContent = label;
        token.appendChild(text);

        var remove = document.createElement("button");
        remove.type = "button";
        remove.className = "btn btn-ghost btn-xs btn-circle";
        remove.setAttribute("aria-label", "Remove " + label + " from repository filters");
        remove.setAttribute("data-repo-remove", value);
        remove.textContent = "×";
        token.appendChild(remove);

        tokenList.appendChild(token);
      });

      input.placeholder = selected.length === 0 ? "Type to add repos" : "Add another repo";
      updateClear();
    }

    function filterOptions() {
      var query = normalizeSearchText(input.value.trim());
      var selectedLookup = selectedSet();
      var matched = 0;
      var shown = 0;

      options.forEach(function (option) {
        var value = optionValue(option);
        var isAllRepos = value === "";
        var alreadySelected = selectedLookup[value];
        var match = false;

        if (isAllRepos) {
          match = query === "";
        } else if (!alreadySelected) {
          match = query === "" || option.dataset.normalizedSearch.indexOf(query) !== -1;
        }

        if (match && !isAllRepos) {
          matched += 1;
        }
        var show = match && shown < MAX_VISIBLE_OPTIONS;
        if (show) {
          shown += 1;
        }
        option.hidden = !show;
      });

      if (empty) {
        empty.classList.toggle("hidden", matched !== 0 || query === "");
      }
      if (status) {
        var totalRepos = Math.max(0, options.length - 1);
        var remaining = Math.max(0, totalRepos - selected.length);
        var shownRepos = visibleOptions().filter(function (option) {
          return optionValue(option) !== "";
        }).length;
        if (query === "") {
          if (selected.length > 0) {
            status.textContent = remaining > shownRepos ? selected.length + " selected. Showing the first " + shownRepos + " of " + remaining + " remaining repositories." : selected.length + " selected. " + remaining + " more repositories available.";
          } else {
            status.textContent = totalRepos > shownRepos ? "Showing the first " + shownRepos + " of " + totalRepos + " repositories. Type to narrow." : totalRepos + " repositories available.";
          }
        } else if (matched > shownRepos) {
          status.textContent = "Showing " + shownRepos + " of " + matched + " matches — keep typing to narrow.";
        } else {
          status.textContent = matched === 1 ? "1 match." : matched + " matches.";
        }
      }

      var visible = visibleOptions();
      if (visible.indexOf(activeOption) === -1) {
        setActive(visible[0] || null);
      }
    }

    function openMenu() {
      filterOptions();
      menu.classList.remove("hidden");
      input.setAttribute("aria-expanded", "true");
    }

    function closeMenu() {
      menu.classList.add("hidden");
      input.setAttribute("aria-expanded", "false");
      setActive(null);
    }

    function applyOption(option) {
      var value = optionValue(option);
      if (value === "") {
        selected = [];
      } else if (selected.indexOf(value) === -1) {
        selected.push(value);
      }
      input.value = "";
      input.setCustomValidity("");
      renderSelected();
      filterOptions();
    }

    function selectOption(option) {
      applyOption(option);
      closeMenu();
      input.focus();
    }

    function commitInput() {
      var text = input.value.trim();
      if (text === "") {
        input.setCustomValidity("");
        return true;
      }

      var exact = findExact(text);
      if (exact) {
        applyOption(exact);
        return true;
      }

      var visibleRepoOptions = visibleOptions().filter(function (option) {
        return optionValue(option) !== "";
      });
      if (visibleRepoOptions.length === 1) {
        applyOption(visibleRepoOptions[0]);
        return true;
      }

      input.setCustomValidity("Choose a repository from the list, or clear this field to search every repository.");
      return false;
    }

    input.addEventListener("focus", openMenu);
    input.addEventListener("input", function () {
      input.setCustomValidity("");
      updateClear();
      openMenu();
    });
    input.addEventListener("keydown", function (event) {
      if (event.key === "Backspace" && input.value === "" && selected.length > 0) {
        selected.pop();
        renderSelected();
        filterOptions();
        return;
      }

      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault();
        openMenu();
        var visible = visibleOptions();
        if (visible.length === 0) {
          return;
        }
        var direction = event.key === "ArrowDown" ? 1 : -1;
        var index = visible.indexOf(activeOption);
        if (index === -1) {
          index = direction === 1 ? -1 : 0;
        }
        setActive(visible[(index + direction + visible.length) % visible.length]);
        return;
      }

      if (event.key === "Enter" && !menu.classList.contains("hidden")) {
        var option = activeOption || visibleOptions()[0];
        if (option) {
          if (input.value.trim() === "" && selected.length === 0 && optionValue(option) === "") {
            closeMenu();
            return;
          }
          event.preventDefault();
          selectOption(option);
        }
        return;
      }

      if (event.key === "Escape") {
        closeMenu();
      }
    });

    tokenList.addEventListener("click", function (event) {
      var button = event.target.closest ? event.target.closest("[data-repo-remove]") : null;
      if (!button) {
        return;
      }
      var value = button.getAttribute("data-repo-remove") || "";
      selected = selected.filter(function (existing) {
        return existing !== value;
      });
      renderSelected();
      filterOptions();
      input.focus();
    });

    options.forEach(function (option) {
      option.addEventListener("mousedown", function (event) {
        event.preventDefault();
      });
      option.addEventListener("click", function () {
        selectOption(option);
      });
    });

    if (clear) {
      clear.addEventListener("click", function (event) {
        event.preventDefault();
        selected = [];
        input.value = "";
        input.setCustomValidity("");
        renderSelected();
        filterOptions();
        closeMenu();
        input.focus();
      });
    }

    var form = root.closest("form");
    if (form) {
      form.addEventListener("submit", function (event) {
        if (!commitInput()) {
          event.preventDefault();
          event.stopPropagation();
          openMenu();
          input.reportValidity();
        }
      });
    }

    document.addEventListener("click", function (event) {
      if (!root.contains(event.target)) {
        closeMenu();
      }
    });

    selected = uniqueValues(Array.from(hiddenContainer.querySelectorAll('input[name="repo"]')).map(function (hidden) {
      return hidden.value;
    }));
    renderSelected();
    filterOptions();
  }

  function initRepoComboboxes(scope) {
    (scope || document).querySelectorAll("[data-repo-combobox]").forEach(initRepoCombobox);
  }

  document.addEventListener("DOMContentLoaded", function () {
    initRepoComboboxes(document);
  });
  document.addEventListener("htmx:afterSwap", function (event) {
    initRepoComboboxes(event.target || document);
  });
})();

// Search results facets: client-side filtering inside long facet lists.
(function () {
  function normalizeSearchText(value) {
    var text = String(value || "").toLowerCase();
    if (text.normalize) {
      text = text.normalize("NFD").replace(/[\u0300-\u036f]/g, "");
    }
    return text;
  }

  document.addEventListener("input", function (event) {
    var input = event.target;
    if (!input || !input.matches || !input.matches("[data-facet-filter]")) {
      return;
    }

    var group = input.closest("[data-facet-group]");
    if (!group) {
      return;
    }

    var query = normalizeSearchText(input.value.trim());
    var visible = 0;
    group.querySelectorAll("[data-facet-value]").forEach(function (value) {
      var haystack = normalizeSearchText(value.getAttribute("data-search") || value.textContent || "");
      var show = query === "" || haystack.indexOf(query) !== -1;
      value.classList.toggle("hidden", !show);
      if (show) {
        visible += 1;
      }
    });

    var empty = group.querySelector("[data-facet-empty]");
    if (empty) {
      empty.classList.toggle("hidden", visible !== 0);
    }
  });
})();

// Browse directory: client-side filter of repo cards by name.
(function () {
  document.addEventListener("input", function (event) {
    var input = event.target;
    if (!input || !input.matches || !input.matches("[data-repo-search]")) {
      return;
    }
    var query = input.value.trim().toLowerCase();
    var grid = document.querySelector("[data-repo-grid]");
    if (!grid) {
      return;
    }
    var anyVisible = false;
    grid.querySelectorAll("[data-repo-card]").forEach(function (card) {
      var name = (card.getAttribute("data-name") || "").toLowerCase();
      var show = query === "" || name.indexOf(query) !== -1;
      card.classList.toggle("hidden", !show);
      if (show) {
        anyVisible = true;
      }
    });
    var empty = document.querySelector("[data-repo-empty]");
    if (empty) {
      empty.classList.toggle("hidden", anyVisible || query === "");
    }
  });
})();

// Slide-over repository management drawer (manage page). Content is loaded into
// #repo-drawer via HTMX; we open the panel once it lands and close on
// backdrop/Escape/close-button.
(function () {
  function root() {
    return document.getElementById("repo-drawer-root");
  }

  function openDrawer() {
    var r = root();
    if (r) {
      r.classList.add("open");
    }
  }

  function closeDrawer() {
    var r = root();
    if (!r) {
      return;
    }
    r.classList.remove("open");
    // Clear stale content after the slide-out transition so it doesn't flash
    // on the next open.
    window.setTimeout(function () {
      var body = document.getElementById("repo-drawer");
      if (body && !r.classList.contains("open")) {
        body.innerHTML = "";
      }
    }, 220);
  }

  document.addEventListener("click", function (event) {
    if (!(event.target instanceof Element)) {
      return;
    }
    if (event.target.closest("[data-drawer-close]")) {
      event.preventDefault();
      closeDrawer();
    }
  });

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") {
      closeDrawer();
    }
  });

  document.addEventListener("htmx:afterSwap", function (event) {
    if (event.target && event.target.id === "repo-drawer" && event.target.children.length > 0) {
      openDrawer();
    }
  });

  // Keep the open drawer in sync with the repo list: drawer/row actions and the
  // 2s indexing poll all swap #repo-list, but the drawer itself used to keep
  // showing stale state until it was reopened. Re-fetch the panel whenever the
  // list re-renders, unless the user is typing in the drawer (branch policy
  // input) or a confirm dialog is open.
  document.addEventListener("htmx:afterSwap", function (event) {
    var target = event.detail && event.detail.target;
    if (!target || target.id !== "repo-list") {
      return;
    }
    var r = root();
    if (!r || !r.classList.contains("open")) {
      return;
    }
    var body = document.getElementById("repo-drawer");
    var panel = body && body.querySelector("[data-drawer-panel]");
    var repoId = panel && panel.getAttribute("data-repo-id");
    if (!repoId) {
      return;
    }
    var active = document.activeElement;
    if (active && body.contains(active) && active.matches("input:not([type=checkbox]):not([type=radio]), textarea, select")) {
      return;
    }
    var confirmDialog = document.getElementById("app-confirm-modal");
    if (confirmDialog && confirmDialog.open) {
      return;
    }
    if (window.htmx) {
      window.htmx.ajax("GET", "/repos/" + repoId + "/panel", { target: "#repo-drawer", swap: "innerHTML" });
    }
  });
})();

// Settings tabs: reflect the active tab in the URL (?tab=) so reloads and
// copied links land on the same tab (the server pre-checks the radio from
// ?tab=). One-shot notice/error params are dropped so they are not replayed,
// and the path is pinned to /settings because the token-create POST renders
// this page from /settings/tokens, which cannot be reloaded as a GET.
(function () {
  document.addEventListener("change", function (event) {
    var tab = event.target;
    if (!(tab instanceof HTMLInputElement) || tab.name !== "settings_tabs" || !tab.value) {
      return;
    }
    var url = new URL(window.location.href);
    url.pathname = "/settings";
    url.searchParams.set("tab", tab.value);
    url.searchParams.delete("notice");
    url.searchParams.delete("error");
    history.replaceState(null, "", url);
  });
})();

// Pretty confirmation dialogs (DaisyUI modal) replacing window.confirm. Wired
// into HTMX via hx-confirm (the htmx:confirm event) and into plain forms via a
// data-confirm attribute. Add data-confirm-variant="danger" for a red button.
(function () {
  var dialog = null;
  var titleEl, msgEl, okBtn, cancelBtn, resolver;

  function ensureDialog() {
    if (dialog) {
      return dialog;
    }
    dialog = document.createElement("dialog");
    dialog.id = "app-confirm-modal";
    dialog.className = "modal";
    dialog.innerHTML =
      '<div class="modal-box">' +
      '<h3 class="text-lg font-semibold" data-confirm-title>Please confirm</h3>' +
      '<p class="py-4 text-sm text-base-content/80" data-confirm-message></p>' +
      '<div class="modal-action">' +
      '<button type="button" class="btn btn-sm btn-ghost" data-confirm-cancel>Cancel</button>' +
      '<button type="button" class="btn btn-sm btn-primary" data-confirm-ok>Confirm</button>' +
      "</div>" +
      "</div>" +
      '<form method="dialog" class="modal-backdrop"><button aria-label="Close">close</button></form>';
    document.body.appendChild(dialog);
    titleEl = dialog.querySelector("[data-confirm-title]");
    msgEl = dialog.querySelector("[data-confirm-message]");
    okBtn = dialog.querySelector("[data-confirm-ok]");
    cancelBtn = dialog.querySelector("[data-confirm-cancel]");
    okBtn.addEventListener("click", function () {
      settle(true);
    });
    cancelBtn.addEventListener("click", function () {
      settle(false);
    });
    // Backdrop click or Escape close the native dialog → treat as cancel.
    dialog.addEventListener("close", function () {
      settle(false);
    });
    return dialog;
  }

  function settle(result) {
    var resolve = resolver;
    resolver = null;
    if (dialog && dialog.open) {
      dialog.close();
    }
    if (resolve) {
      resolve(result);
    }
  }

  function showConfirm(message, danger) {
    ensureDialog();
    // Settle any in-flight prompt as cancelled before showing a new one.
    if (resolver) {
      settle(false);
    }
    msgEl.textContent = message;
    okBtn.className = "btn btn-sm " + (danger ? "btn-error" : "btn-primary");
    return new Promise(function (resolve) {
      resolver = resolve;
      dialog.showModal();
      okBtn.focus();
    });
  }

  function isDanger(el) {
    return !!(el && el.getAttribute && el.getAttribute("data-confirm-variant") === "danger");
  }

  // HTMX requests carrying hx-confirm. detail.question is the resolved message.
  document.addEventListener("htmx:confirm", function (event) {
    var question = event.detail.question;
    if (!question) {
      return; // no hx-confirm on this element → let the request proceed
    }
    event.preventDefault();
    var el = event.detail.elt;
    showConfirm(question, isDanger(el)).then(function (ok) {
      if (ok) {
        event.detail.issueRequest(true);
      } else if (el && el.matches && el.matches('input[type="checkbox"]')) {
        // The user toggled the control optimistically; put it back on cancel.
        el.checked = !el.checked;
      }
    });
  });

  // Plain (non-HTMX) forms using data-confirm.
  document.addEventListener(
    "submit",
    function (event) {
      var form = event.target;
      if (!(form instanceof HTMLFormElement)) {
        return;
      }
      var question = form.getAttribute("data-confirm");
      if (!question) {
        return;
      }
      if (form.dataset.confirmed === "1") {
        form.dataset.confirmed = "";
        return; // confirmed → allow native submission
      }
      event.preventDefault();
      showConfirm(question, isDanger(form)).then(function (ok) {
        if (!ok) {
          return;
        }
        form.dataset.confirmed = "1";
        if (typeof form.requestSubmit === "function") {
          form.requestSubmit();
        } else {
          form.submit();
        }
      });
    },
    true
  );
})();
