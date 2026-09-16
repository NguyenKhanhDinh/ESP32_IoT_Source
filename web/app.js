console.log("app.js loaded");

const eventSource = new EventSource("/api/telemetry");

eventSource.onopen = function () {
  console.log("SSE connected");

  document.getElementById("status").textContent = "Realtime connected";
};

eventSource.onmessage = function (event) {
  console.log("SSE message:", event.data);

  try {
    const data = JSON.parse(event.data);

    document.getElementById("temperature").textContent = Number(
      data.temperature,
    ).toFixed(2);

    document.getElementById("humidity").textContent = Number(
      data.humidity,
    ).toFixed(2);

    document.getElementById("light").textContent = Number(data.light).toFixed(
      2,
    );

    document.getElementById("updated").textContent =
      new Date().toLocaleTimeString();

    document.getElementById("status").textContent = "Realtime connected";
  } catch (error) {
    console.error("JSON error:", error);
  }
};

eventSource.onerror = function (error) {
  console.error("SSE connection error:", error);

  document.getElementById("status").textContent = "Realtime connection lost";
};
