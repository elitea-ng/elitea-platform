// Runs cap.sh against the extracted mounts, before actions/cache saves them.
const { execFileSync } = require("child_process");
const path = require("path");
execFileSync("bash", [path.join(__dirname, "cap.sh")], {
  stdio: "inherit",
  env: {
    ...process.env,
    MOUNTS_DIR: process.env["INPUT_PATH"],
    CAP_MB: process.env["INPUT_CAP-MB"],
  },
});
