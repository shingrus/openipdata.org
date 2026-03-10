(function () {
  const root = document.querySelector("[data-geofeeds-root]");

  if (!(root instanceof HTMLElement)) {
    return;
  }

  if (root.dataset.dbConfigured !== "true") {
    return;
  }

  const apiUrl = root.dataset.apiUrl;
  const countNode = root.querySelector("[data-geofeeds-count]");
  const dumpLinkNode = root.querySelector("[data-geofeeds-dump-link]");
  const stateNode = root.querySelector("[data-geofeeds-state]");
  const tableWrapNode = root.querySelector("[data-geofeeds-table-wrap]");
  const bodyNode = root.querySelector("[data-geofeeds-body]");

  if (
    typeof apiUrl !== "string"
    || !(countNode instanceof HTMLElement)
    || !(dumpLinkNode instanceof HTMLElement)
    || !(stateNode instanceof HTMLElement)
    || !(tableWrapNode instanceof HTMLElement)
    || !(bodyNode instanceof HTMLElement)
  ) {
    return;
  }
  const dateFormatter = new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric"
  });
  const dateTimeFormatter = new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    month: "short",
    second: "2-digit",
    timeZoneName: "short",
    year: "numeric"
  });

  function setCount(count) {
    countNode.textContent = count.toLocaleString("en-US");
  }

  function showDumpLink() {
    dumpLinkNode.classList.remove("is-hidden");
  }

  function hideDumpLink() {
    dumpLinkNode.classList.add("is-hidden");
  }

  function showState(text) {
    stateNode.textContent = text;
    stateNode.classList.remove("is-hidden");
    tableWrapNode.classList.add("is-hidden");
  }

  function showTable() {
    stateNode.classList.add("is-hidden");
    tableWrapNode.classList.remove("is-hidden");
  }

  function createLastSuccessNode(value) {
    const wrapper = document.createElement("span");

    wrapper.className = "last-success";

    if (typeof value !== "string") {
      wrapper.textContent = "Never";
      return wrapper;
    }

    const date = new Date(value);

    if (Number.isNaN(date.getTime())) {
      wrapper.textContent = "Unknown";
      return wrapper;
    }

    const dateText = document.createElement("span");
    const fullLabel = dateTimeFormatter.format(date);

    dateText.className = "last-success-date";
    dateText.textContent = dateFormatter.format(date);
    dateText.title = fullLabel;
    dateText.setAttribute("aria-label", fullLabel);
    wrapper.appendChild(dateText);

    return wrapper;
  }

  function renderRows(rows) {
    const fragment = document.createDocumentFragment();

    rows.forEach((row) => {
      if (!row || typeof row.url !== "string") {
        return;
      }

      const tableRow = document.createElement("tr");
      const urlCell = document.createElement("td");
      const lastSuccessCell = document.createElement("td");
      const link = document.createElement("a");

      urlCell.className = "geofeed-url";
      lastSuccessCell.className = "geofeed-last-success";

      link.href = row.url;
      link.rel = "nofollow noopener noreferrer";
      link.textContent = row.url;

      urlCell.appendChild(link);
      lastSuccessCell.appendChild(createLastSuccessNode(row.last_success_at));
      tableRow.appendChild(urlCell);
      tableRow.appendChild(lastSuccessCell);
      fragment.appendChild(tableRow);
    });

    bodyNode.replaceChildren(fragment);
  }

  async function loadGeofeeds() {
    try {
      const response = await fetch(apiUrl, {
        headers: {
          Accept: "application/json"
        }
      });

      const data = await response.json();

      if (!response.ok) {
        throw new Error(`request failed with status ${response.status}`);
      }

      if (!Array.isArray(data) || data.length === 0) {
        setCount(0);
        showDumpLink();
        showState("No geofeeds were returned.");
        return;
      }

      setCount(data.length);
      showDumpLink();
      renderRows(data);
      showTable();
    } catch (_error) {
      setCount(0);
      hideDumpLink();
      showState("Geofeed data is temporarily unavailable.");
    }
  }

  void loadGeofeeds();
}());
