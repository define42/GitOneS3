// Reuse the guarded bootstrap as a fallback if its early request failed.
import "./theme-init.js";
import React from "react";
import ReactDOM from "react-dom/client";
import { App } from "./App";
import "./style.css";

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
