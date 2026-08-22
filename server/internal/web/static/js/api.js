// Small fetch wrapper shared by every page: JSON in/out, credentials
// included for the session cookie, automatic redirect to /login on 401.
async function api(method, path, body) {
	const opts = {
		method,
		headers: { "Content-Type": "application/json" },
		credentials: "same-origin",
	};
	if (body !== undefined) opts.body = JSON.stringify(body);
	const res = await fetch(path, opts);
	if (res.status === 401 && path !== "/api/auth/login") {
		window.location.href = "/login";
		throw new Error("non authentifié");
	}
	let data = null;
	try { data = await res.json(); } catch (e) { /* no body */ }
	if (!res.ok) {
		const msg = (data && data.error) ? data.error : `erreur ${res.status}`;
		throw new Error(msg);
	}
	return data;
}

function toast(message, kind) {
	const el = document.createElement("div");
	el.className = "toast" + (kind ? " " + kind : "");
	el.textContent = message;
	document.body.appendChild(el);
	setTimeout(() => el.remove(), 4500);
}

function fmtBytes(bytes) {
	if (bytes === null || bytes === undefined) return "—";
	const units = ["o", "Ko", "Mo", "Go", "To"];
	let v = bytes, i = 0;
	while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
	return (i === 0 ? v.toFixed(0) : v.toFixed(2)) + " " + units[i];
}

function fmtDate(iso) {
	if (!iso) return "jamais";
	const d = new Date(iso);
	if (isNaN(d.getTime())) return "jamais";
	return d.toLocaleString("fr-FR", { dateStyle: "medium", timeStyle: "short" });
}

function fmtRelative(iso) {
	if (!iso) return "jamais";
	const d = new Date(iso);
	if (isNaN(d.getTime())) return "jamais";
	const diffSec = Math.round((Date.now() - d.getTime()) / 1000);
	if (diffSec < 60) return "à l'instant";
	if (diffSec < 3600) return Math.floor(diffSec / 60) + " min";
	if (diffSec < 86400) return Math.floor(diffSec / 3600) + " h";
	return Math.floor(diffSec / 86400) + " j";
}

function statusBadge(status) {
	const labels = { success: "réussie", failed: "échouée", running: "en cours", cancelled: "annulée" };
	return `<span class="badge ${status}"><span class="dot"></span>${labels[status] || status}</span>`;
}

function renderSidebar(active) {
	const items = [
		["/", "Tableau de bord"],
		["/devices", "Appareils"],
		["/settings", "Paramètres"],
	];
	const links = items.map(([href, label]) => {
		const isActive = (href === "/" && active === "dashboard") ||
			(href === "/devices" && active === "devices") ||
			(href === "/settings" && active === "settings");
		return `<a class="nav-link${isActive ? " active" : ""}" href="${href}">${label}</a>`;
	}).join("");
	return `
	<div class="sidebar">
		<div class="brand"><span class="dot"></span>Backup Center</div>
		${links}
		<div class="nav-spacer"></div>
		<a class="nav-link" href="#" id="logout-link">Déconnexion</a>
	</div>`;
}

function wireLogout() {
	const el = document.getElementById("logout-link");
	if (!el) return;
	el.addEventListener("click", async (e) => {
		e.preventDefault();
		try { await api("POST", "/api/auth/logout"); } catch (e) {}
		window.location.href = "/login";
	});
}
