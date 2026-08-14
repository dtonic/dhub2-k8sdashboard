import { expect, it } from "vitest";
import { acquireSingleFlight } from "./singleFlight";

it("allows exactly one rapid mutation until release",()=>{const gate={current:false};const first=acquireSingleFlight(gate);const second=acquireSingleFlight(gate);expect(first).toBeTypeOf("function");expect(second).toBeNull();first!();expect(acquireSingleFlight(gate)).toBeTypeOf("function")});
