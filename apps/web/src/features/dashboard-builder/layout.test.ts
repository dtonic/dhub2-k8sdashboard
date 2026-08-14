import { describe, expect, it } from "vitest";
import type { DashboardDefinition } from "@k8s-dashboard/dashboard-schema";
import { placeWidget, updateWidgetLayout } from "./layout";

const definition: DashboardDefinition = {schemaVersion:1,id:"draft",title:"Draft",variables:[],widgets:[{id:"a",title:"A",type:"Stat",binding:"nodes.ready",layout:{x:0,y:0,w:3,h:2}}]};
describe("builder layout",()=>{
  it("deterministically rejects overlap and bounds",()=>{expect(updateWidgetLayout(definition,"a",{x:10,y:0,w:3,h:2})).toBeNull();const withB={...definition,widgets:[...definition.widgets,{id:"b",title:"B",type:"Gauge",binding:"pods.runningPercent",layout:{x:3,y:0,w:3,h:2}}] as DashboardDefinition["widgets"]};expect(updateWidgetLayout(withB,"a",{x:2,y:0,w:3,h:2})).toBeNull();expect(updateWidgetLayout(withB,"a",{x:0,y:2,w:3,h:2})?.widgets[0].layout.y).toBe(2)});
  it("places at the first free grid cell",()=>expect(placeWidget(definition.widgets,3,2)).toEqual({x:3,y:0,w:3,h:2}));
  it("scans a fixed grid for the maximum 24 widgets",()=>{const widgets=Array.from({length:24},(_,i)=>({...definition.widgets[0],id:`w${i}`,layout:{x:(i%6)*2,y:Math.floor(i/6)*2,w:2,h:2}}));for(let i=0;i<1000;i++) expect(placeWidget(widgets,2,2)).not.toBeNull()});
});
