// Tiny dependency-free canvas charts. Deliberately minimal: this panel is
// meant to run entirely offline on a LAN, so no charting library is
// fetched from anywhere - a few dozen lines of canvas drawing is enough
// for "storage per device" and "backups per day".

function setupCanvas(canvas) {
	const dpr = window.devicePixelRatio || 1;
	const rect = canvas.getBoundingClientRect();
	canvas.width = rect.width * dpr;
	canvas.height = rect.height * dpr;
	const ctx = canvas.getContext("2d");
	ctx.scale(dpr, dpr);
	return { ctx, w: rect.width, h: rect.height };
}

function drawBarChart(canvas, labels, values, opts) {
	opts = opts || {};
	const { ctx, w, h } = setupCanvas(canvas);
	ctx.clearRect(0, 0, w, h);
	if (!values.length) {
		ctx.fillStyle = "#8b98a9";
		ctx.font = "13px sans-serif";
		ctx.fillText("Aucune donnée", 8, h / 2);
		return;
	}
	const padBottom = 24, padTop = 10, padLeft = 8, padRight = 8;
	const max = Math.max(...values, 1);
	const barGap = 10;
	const barW = (w - padLeft - padRight - barGap * (values.length - 1)) / values.length;

	values.forEach((v, i) => {
		const barH = ((h - padTop - padBottom) * v) / max;
		const x = padLeft + i * (barW + barGap);
		const y = h - padBottom - barH;
		const grad = ctx.createLinearGradient(0, y, 0, h - padBottom);
		grad.addColorStop(0, "#4f8cff");
		grad.addColorStop(1, "#7c5cff");
		ctx.fillStyle = grad;
		roundRect(ctx, x, y, barW, Math.max(barH, 2), 4);
		ctx.fill();

		ctx.fillStyle = "#8b98a9";
		ctx.font = "11px sans-serif";
		ctx.textAlign = "center";
		const label = labels[i] && labels[i].length > 10 ? labels[i].slice(0, 9) + "…" : (labels[i] || "");
		ctx.fillText(label, x + barW / 2, h - 8);
	});
	ctx.textAlign = "left";
}

function drawLineChart(canvas, labels, values) {
	const { ctx, w, h } = setupCanvas(canvas);
	ctx.clearRect(0, 0, w, h);
	if (!values.length) {
		ctx.fillStyle = "#8b98a9";
		ctx.font = "13px sans-serif";
		ctx.fillText("Aucune donnée", 8, h / 2);
		return;
	}
	const padBottom = 24, padTop = 14, padLeft = 10, padRight = 10;
	const max = Math.max(...values, 1);
	const stepX = (w - padLeft - padRight) / Math.max(values.length - 1, 1);

	ctx.beginPath();
	values.forEach((v, i) => {
		const x = padLeft + i * stepX;
		const y = padTop + (h - padTop - padBottom) * (1 - v / max);
		if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
	});
	ctx.strokeStyle = "#4f8cff";
	ctx.lineWidth = 2;
	ctx.stroke();

	ctx.lineTo(padLeft + (values.length - 1) * stepX, h - padBottom);
	ctx.lineTo(padLeft, h - padBottom);
	ctx.closePath();
	const grad = ctx.createLinearGradient(0, padTop, 0, h - padBottom);
	grad.addColorStop(0, "rgba(79,140,255,.25)");
	grad.addColorStop(1, "rgba(79,140,255,0)");
	ctx.fillStyle = grad;
	ctx.fill();

	ctx.fillStyle = "#8b98a9";
	ctx.font = "11px sans-serif";
	ctx.textAlign = "center";
	const step = Math.ceil(labels.length / 7) || 1;
	labels.forEach((l, i) => {
		if (i % step !== 0 && i !== labels.length - 1) return;
		const x = padLeft + i * stepX;
		ctx.fillText(l.slice(5), x, h - 8);
	});
	ctx.textAlign = "left";
}

function roundRect(ctx, x, y, w, h, r) {
	ctx.beginPath();
	ctx.moveTo(x + r, y);
	ctx.arcTo(x + w, y, x + w, y + h, r);
	ctx.arcTo(x + w, y + h, x, y + h, r);
	ctx.arcTo(x, y + h, x, y, r);
	ctx.arcTo(x, y, x + w, y, r);
	ctx.closePath();
}
