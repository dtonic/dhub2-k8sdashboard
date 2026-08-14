export function acquireSingleFlight(gate:{current:boolean}):(()=>void)|null {
  if(gate.current)return null;
  gate.current=true;
  return ()=>{gate.current=false};
}
