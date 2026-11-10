import fs from "node:fs";
export const ok = fs.readFileSync("/etc/hostname", "utf8");
