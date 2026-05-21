import Header from '@/components/Header';
import Hero from '@/components/Hero';
import Strip from '@/components/Strip';
import HCLBlock from '@/components/HCLBlock';
import HowItWorks from '@/components/HowItWorks';
import CLICheats from '@/components/CLICheats';
import Lifecycle from '@/components/Lifecycle';
import Stack from '@/components/Stack';
import FAQ from '@/components/FAQ';
import EndCTA from '@/components/EndCTA';
import Footer from '@/components/Footer';

export default function Home() {
  return (
    <>
      <Header />

      <main>
        <Hero />
        <Strip />
        <HCLBlock />
        <HowItWorks />
        <CLICheats />
        <Lifecycle />
        <Stack />
        <FAQ />
        <EndCTA />
      </main>
      <Footer />
    </>
  );
}
