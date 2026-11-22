/* NestNAS landing page: small, dependency-free, and respectful of reduced-motion. */
(function () {
  "use strict";
  var root = document.documentElement;
  var reduce = matchMedia("(prefers-reduced-motion: reduce)").matches || /static/.test(location.search);
  var fine = matchMedia("(hover: hover) and (pointer: fine)").matches;
  var $ = function (s, c) { return (c || document).querySelector(s); };
  var $$ = function (s, c) { return Array.prototype.slice.call((c || document).querySelectorAll(s)); };
  var rand = function (a, b) { return a + Math.random() * (b - a); };
  var started = false;

  /* ---------- boot splash ---------- */
  function finishBoot() {
    if (started) return; started = true;
    var sp = $("#splash");
    try { sessionStorage.setItem("nest-booted", "1"); } catch (e) {}
    if (sp) { sp.classList.add("out"); setTimeout(function () { root.classList.remove("booting"); }, 700); }
    else root.classList.remove("booting");
    startScene();
  }
  function bootSequence() {
    var pct = $("#splash-pct"), fill = $("#splash-fill"), t0 = performance.now(), dur = 1500;
    $("#splash-skip").addEventListener("click", finishBoot);
    $("#splash").addEventListener("click", finishBoot);
    (function tick(now) {
      if (started) return;
      var p = Math.min(1, (now - t0) / dur), e = 1 - Math.pow(1 - p, 2.2), v = Math.round(e * 100);
      pct.textContent = v; fill.style.width = v + "%";
      if (p < 1) requestAnimationFrame(tick); else setTimeout(finishBoot, 220);
    })(t0);
  }

  /* ---------- reveal on scroll ---------- */
  function setupReveal() {
    $$(".bento .cell, .stats .stat, .steps .step, .principles li, .fine-list").forEach(function (el, i) {
      el.style.setProperty("--d", ((i % 4) * 0.08) + "s");
    });
    var els = $$("[data-reveal]");
    if (!("IntersectionObserver" in window)) { els.forEach(function (e) { e.classList.add("in"); }); return; }
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) { if (en.isIntersecting) { en.target.classList.add("in"); io.unobserve(en.target); } });
    }, { threshold: 0.12, rootMargin: "0px 0px -6% 0px" });
    els.forEach(function (e) { io.observe(e); });
  }

  /* ---------- custom cursor + magnetic buttons + card spotlight ---------- */
  function setupPointer() {
    if (!fine || reduce) return;
    var cur = $("#cursor"), x = 0, y = 0, cx = 0, cy = 0, ring = $("b", cur), dot = $("i", cur);
    document.body.classList.add("has-cursor");
    addEventListener("mousemove", function (e) { x = e.clientX; y = e.clientY; dot.style.transform = "translate(" + x + "px," + y + "px)"; }, { passive: true });
    (function loop() { cx += (x - cx) * 0.18; cy += (y - cy) * 0.18; ring.style.transform = "translate(" + cx + "px," + cy + "px)"; requestAnimationFrame(loop); })();
    $$("a, button, input, .cell, [data-magnetic]").forEach(function (el) {
      el.addEventListener("mouseenter", function () { cur.classList.add("hot"); });
      el.addEventListener("mouseleave", function () { cur.classList.remove("hot"); });
    });
    $$("[data-magnetic]").forEach(function (el) {
      el.addEventListener("mousemove", function (e) {
        var r = el.getBoundingClientRect();
        el.style.transform = "translate(" + ((e.clientX - r.left - r.width / 2) * 0.22) + "px," + ((e.clientY - r.top - r.height / 2) * 0.3) + "px)";
      });
      el.addEventListener("mouseleave", function () { el.style.transform = ""; });
    });
    $$(".cell").forEach(function (c) {
      c.addEventListener("mousemove", function (e) {
        var r = c.getBoundingClientRect();
        c.style.setProperty("--mx", (e.clientX - r.left) + "px"); c.style.setProperty("--my", (e.clientY - r.top) + "px");
      });
    });
  }

  /* ---------- nav scroll spy ---------- */
  function setupSpy() {
    var links = $$(".links a"), map = {};
    links.forEach(function (a) { map[a.getAttribute("href").slice(1)] = a; });
    if (!("IntersectionObserver" in window)) return;
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (en.isIntersecting) { links.forEach(function (l) { l.classList.remove("on"); }); if (map[en.target.id]) map[en.target.id].classList.add("on"); }
      });
    }, { rootMargin: "-45% 0px -50% 0px" });
    Object.keys(map).forEach(function (id) { var s = document.getElementById(id); if (s) io.observe(s); });
  }

  /* ---------- hero scene: ring + phone counter ---------- */
  function startScene() {
    var val = $("#ring-val"), n = $("#ring-n");
    if (val) { requestAnimationFrame(function () { val.style.strokeDasharray = "38 100"; }); }
    if (n) countTo(n, 38, 1800, "%");
    var bk = $("#bk-count"), bar = $("#bk-bar"), total = 3902;
    if (!bk) return;
    if (reduce) { bk.textContent = "1,284"; bar.style.width = "33%"; return; }
    var t0 = performance.now(), dur = 15000, hold = 2600;
    (function loop(now) {
      var t = (now - t0) % (dur + hold), p = Math.min(1, t / dur), e = p < 1 ? 1 - Math.pow(1 - p, 1.6) : 1;
      var v = Math.round(e * total);
      bk.textContent = v.toLocaleString("en-US"); bar.style.width = (v / total * 100) + "%";
      requestAnimationFrame(loop);
    })(t0);
  }
  function countTo(el, target, ms, suffix, decimals) {
    if (reduce) { el.textContent = (decimals ? target.toFixed(decimals) : Math.round(target).toLocaleString("en-US")) + (suffix || ""); return; }
    var t0 = performance.now();
    (function step(now) {
      var p = Math.min(1, (now - t0) / ms), e = 1 - Math.pow(1 - p, 3), v = target * e;
      el.textContent = (decimals ? v.toFixed(decimals) : Math.round(v).toLocaleString("en-US")) + (suffix || "");
      if (p < 1) requestAnimationFrame(step);
    })(t0);
  }

  /* ---------- receipts counters ---------- */
  function setupCounters() {
    var els = $$("[data-count]");
    if (!("IntersectionObserver" in window) || reduce) {
      els.forEach(function (el) { var t = +el.dataset.count; el.textContent = (el.dataset.decimals ? t.toFixed(+el.dataset.decimals) : t.toLocaleString("en-US")) + (el.dataset.suffix || ""); });
      return;
    }
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (!en.isIntersecting) return; io.unobserve(en.target);
        var el = en.target, t = +el.dataset.count;
        if (t === 0) { el.textContent = "0"; return; }
        countTo(el, t, 1600, el.dataset.suffix || "", el.dataset.decimals ? +el.dataset.decimals : 0);
      });
    }, { threshold: 0.6 });
    els.forEach(function (el) { io.observe(el); });
  }

  /* ---------- backup visual ---------- */
  function setupBackupViz() {
    var host = $("#backup-viz"); if (!host) return;
    var tiles = $("#bv-tiles"), fill = $("#bv-fill"), num = $("#bv-num"), n = 60, cells = [];
    var hues = ["#f2a65a", "#a8e0d6", "#e8987a", "#cfe0b4", "#9aa8e8", "#f7c08a"];
    for (var i = 0; i < n; i++) { var c = document.createElement("i"); tiles.appendChild(c); cells.push(c); }
    function paint(k) {
      var fresh = Math.round(k * 0.31), had = k - fresh;
      cells.forEach(function (c, i) { if (i < k) { c.style.background = i % 3 === 0 ? hues[i % hues.length] : "#3a342d"; c.classList.add("done"); } else { c.style.background = ""; c.classList.remove("done"); } });
      fill.style.width = (k / n * 100) + "%";
      num.textContent = (fresh * 4) + " new · " + (had * 9) + " already there";
    }
    if (reduce) { paint(40); return; }
    var running = false, k = 0, timer;
    function run() { clearInterval(timer); k = 0; timer = setInterval(function () { k++; paint(k); if (k >= n) { clearInterval(timer); setTimeout(function () { if (running) run(); }, 2600); } }, 150); }
    new IntersectionObserver(function (es) { es.forEach(function (e) { running = e.isIntersecting; if (running) run(); else clearInterval(timer); }); }, { threshold: 0.3 }).observe(host);
  }

  /* ---------- encryption scramble ---------- */
  function setupCipher() {
    var host = $("#cipher"), txt = $("#cipher-text"), btn = $("#cipher-btn"), label = $("#cipher-label"); if (!host) return;
    var plain = "FF D8 FF E0 00 10 4A 46 49 46 00 01 01 00 00 01\n00 01 00 00 FF DB 00 43 00 08 06 06 07 06 05 08";
    function hex(len) { var s = ""; for (var i = 0; i < len; i++) s += ("0" + Math.floor(Math.random() * 256).toString(16)).slice(-2).toUpperCase() + " "; return s.trim(); }
    var cipher = "4E 4E 45 31 01 10 00 00 " + hex(8) + "\n" + hex(24);
    var locked = false, busy = false, user = false;
    function scramble(to, done) {
      if (reduce) { txt.textContent = to; if (done) done(); return; }
      busy = true; var t0 = performance.now(), dur = 700;
      (function f(now) {
        var p = Math.min(1, (now - t0) / dur), out = "";
        for (var i = 0; i < to.length; i++) { var ch = to[i]; out += (ch === " " || ch === "\n" || i < p * to.length) ? ch : "0123456789ABCDEF"[Math.floor(Math.random() * 16)]; }
        txt.textContent = out;
        if (p < 1) requestAnimationFrame(f); else { busy = false; if (done) done(); }
      })(t0);
    }
    function set(l) {
      if (busy) return; locked = l;
      host.classList.toggle("locked", l); btn.setAttribute("aria-pressed", l);
      label.textContent = l ? "ciphertext (as stored on disk)" : "plaintext (as used by you)";
      scramble(l ? cipher : plain);
    }
    btn.addEventListener("click", function () { user = true; set(!locked); });
    var vis = false; new IntersectionObserver(function (es) { es.forEach(function (e) { vis = e.isIntersecting; }); }).observe(host);
    if (!reduce) setInterval(function () { if (vis && !user) set(!locked); }, 4800);
  }

  /* ---------- terminal copy ---------- */
  function setupCopy() {
    var b = $("#copy"); if (!b) return;
    var cmd = "git clone https://github.com/Saransh-Jainbu/nestnas.git\ncd nestnas && make build\n./nestnas service install";
    b.addEventListener("click", function () { copyText(cmd, function () { b.textContent = "copied"; setTimeout(function () { b.textContent = "copy"; }, 1800); }); });
  }
  function copyText(t, ok) {
    if (navigator.clipboard) navigator.clipboard.writeText(t).then(ok, function () {});
    else { var a = document.createElement("textarea"); a.value = t; document.body.appendChild(a); a.select(); try { document.execCommand("copy"); ok(); } catch (e) {} a.remove(); }
  }

  /* ---------- command palette ---------- */
  function setupPalette() {
    var pal = $("#palette"), input = $("#pal-input"), list = $("#pal-list"), sel = 0, shown = [];
    var items = [
      { t: "Features", s: "what it does", go: "#features" }, { t: "Security", s: "how it is protected", go: "#trust" },
      { t: "Metrics", s: "project numbers", go: "#receipts" }, { t: "Get started", s: "build & run", go: "#start" },
      { t: "Limitations", s: "known limitations", go: "#fine" },
      { t: "Open the GitHub repo", s: "↗", url: "https://github.com/Saransh-Jainbu/nestnas" },
      { t: "Read the security model", s: "↗", url: "https://github.com/Saransh-Jainbu/nestnas/blob/main/SECURITY.md" },
      { t: "Copy the install commands", s: "clipboard", copy: true },
      { t: "About the author", s: "↗ saranshjain.dev", url: "https://www.saranshjain.dev/" }
    ];
    function render() {
      var q = input.value.trim().toLowerCase();
      shown = items.filter(function (i) { return !q || (i.t + " " + i.s).toLowerCase().indexOf(q) > -1; });
      sel = Math.min(sel, Math.max(0, shown.length - 1));
      list.innerHTML = "";
      shown.forEach(function (it, i) {
        var li = document.createElement("li"); li.setAttribute("role", "option"); if (i === sel) li.className = "sel";
        li.innerHTML = "<span></span><small></small>"; li.firstChild.textContent = it.t; li.lastChild.textContent = it.s;
        li.addEventListener("click", function () { run(it); }); list.appendChild(li);
      });
    }
    function run(it) {
      close();
      if (it.go) document.querySelector(it.go).scrollIntoView({ behavior: reduce ? "auto" : "smooth" });
      else if (it.url) window.open(it.url, "_blank", "noopener");
      else if (it.copy) copyText("git clone https://github.com/Saransh-Jainbu/nestnas.git\ncd nestnas && make build\n./nestnas service install", function () {});
    }
    function open() { pal.hidden = false; input.value = ""; sel = 0; render(); setTimeout(function () { input.focus(); }, 20); }
    function close() { pal.hidden = true; }
    $("#open-palette").addEventListener("click", open);
    pal.addEventListener("click", function (e) { if (e.target === pal) close(); });
    input.addEventListener("input", function () { sel = 0; render(); });
    addEventListener("keydown", function (e) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); pal.hidden ? open() : close(); return; }
      if (pal.hidden) return;
      if (e.key === "Escape") close();
      else if (e.key === "ArrowDown") { e.preventDefault(); sel = (sel + 1) % shown.length; render(); }
      else if (e.key === "ArrowUp") { e.preventDefault(); sel = (sel - 1 + shown.length) % shown.length; render(); }
      else if (e.key === "Enter" && shown[sel]) run(shown[sel]);
    });
  }

  /* ---------- go ---------- */
  setupReveal(); setupPointer(); setupSpy(); setupCounters(); setupBackupViz(); setupCipher(); setupCopy(); setupPalette();
  if (root.classList.contains("booting")) bootSequence(); else { started = true; startScene(); }
})();
