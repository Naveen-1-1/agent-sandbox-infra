import { tone } from "../fleet.ts";

export function Phase({ phase }: { phase: string }) {
  return <span className={`phase t-${tone(phase)}`}>{phase || "Pending"}</span>;
}
